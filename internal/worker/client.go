package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/ssh"
)

// Client implements the worker boundary; every operation uses HTTP, never another SSH exec session.
type Client struct {
	master *ssh.Master
	http   *http.Client
}

func Dial(ctx context.Context, c Config, services map[string]string, sshConfig, server string) (*Client, error) {
	m, err := ssh.Dial(ctx, c.Host, sshConfig, initialization{protocol, c, server, services}, executable)
	if err != nil {
		return nil, err
	}
	for name, address := range services {
		forward, err := c.ServiceForward(name, address)
		if err == nil {
			err = m.Forward(ctx, forward)
		}
		if err != nil {
			return nil, errors.Join(err, m.Close())
		}
	}
	client := socketClient(m.Socket)
	client.master = m
	return client, nil
}

func socketClient(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
		MaxIdleConns: 8, MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute,
	}
	return &Client{http: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("worker redirected a request") }}}
}

// Packaged builds include matching Linux binaries for both worker architectures. A native source
// build can deploy itself; the internal worker entry point does not start the server or open its DB.
func executable(machine string) (string, error) {
	arch, ok := architectures[machine]
	if !ok {
		return "", fmt.Errorf("unsupported worker architecture %q", machine)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return "", err
	}
	bundled := filepath.Join(filepath.Dir(exe), "..", "libexec", "ordo", "ordo-"+arch)
	if info, err := os.Stat(bundled); err == nil && info.Mode().IsRegular() {
		return bundled, nil
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == arch {
		return exe, nil
	}
	return "", fmt.Errorf("missing Linux %s worker executable; use a packaged Ordo build", arch)
}

func (c *Client) Close() error {
	c.http.CloseIdleConnections()
	if c.master != nil {
		return c.master.Close()
	}
	return nil
}

func connectionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("%w: %v", ssh.ErrTransport, err)
}

func (c *Client) call(ctx context.Context, operation string, q request, value any) error {
	data, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if len(data) > maxControl {
		return errors.New("worker control request exceeds 64 MiB")
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://ordo-worker/rpc/"+operation, bytes.NewReader(data))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(r)
	if err != nil {
		return connectionError(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err = io.ReadAll(io.LimitReader(response.Body, maxControl+1))
	if err != nil {
		return connectionError(ctx, err)
	}
	if len(data) > maxControl {
		return connectionError(ctx, errors.New("worker response exceeds limit"))
	}
	if response.StatusCode != http.StatusOK {
		var result result
		if err := json.Unmarshal(data, &result); err != nil || result.err() == nil {
			return connectionError(ctx, fmt.Errorf("invalid worker error (HTTP %d)", response.StatusCode))
		}
		return result.err()
	}
	if value == nil {
		return nil
	}
	if err := json.Unmarshal(data, value); err != nil {
		return connectionError(ctx, err)
	}
	return nil
}

// readFailure separates disk/consumer errors from interrupted HTTP streams.
type readFailure struct {
	io.Reader
	mu      sync.Mutex
	failure error
}

func (r *readFailure) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		r.mu.Lock()
		r.failure = err
		r.mu.Unlock()
	}
	return n, err
}
func (r *readFailure) err() error { r.mu.Lock(); defer r.mu.Unlock(); return r.failure }

func (c *Client) stream(ctx context.Context, method, path string, input io.Reader, consume func(io.Reader) error) error {
	var source *readFailure
	if input != nil {
		source = &readFailure{Reader: input}
		input = source
	}
	r, err := http.NewRequestWithContext(ctx, method, "http://ordo-worker/"+path, input)
	if err != nil {
		return err
	}
	response, err := c.http.Do(r)
	if err != nil {
		if source != nil && source.err() != nil {
			return source.err()
		}
		return connectionError(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		var result result
		if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result); err != nil || result.err() == nil {
			return connectionError(ctx, fmt.Errorf("invalid worker response (HTTP %d)", response.StatusCode))
		}
		return result.err()
	}
	body := &readFailure{Reader: response.Body}
	if consume != nil {
		err = consume(body)
	}
	if err == nil {
		_, err = io.Copy(io.Discard, body)
	}
	if body.err() != nil {
		return connectionError(ctx, body.err())
	}
	if err != nil {
		return err
	}
	if source != nil && source.err() != nil {
		return source.err()
	}
	data, err := base64.StdEncoding.DecodeString(response.Trailer.Get(resultTrailer))
	var result result
	if err != nil || json.Unmarshal(data, &result) != nil {
		return connectionError(ctx, errors.New("worker stream ended without a valid result"))
	}
	return result.err()
}

func (c *Client) upload(ctx context.Context, target, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return c.stream(ctx, http.MethodPut, target, f, nil)
}

func (c *Client) Probe(ctx context.Context) (Probe, error) {
	var v Probe
	err := c.call(ctx, "probe", request{}, &v)
	return v, err
}
func (c *Client) Poll(ctx context.Context) (Snapshot, error) {
	var v Snapshot
	err := c.call(ctx, "poll", request{}, &v)
	return v, err
}
func (c *Client) GPUBusy(ctx context.Context, gpus []pool.GPU) ([]string, error) {
	var v []string
	err := c.call(ctx, "gpu", request{GPUs: gpus}, &v)
	return v, err
}
func (c *Client) Launch(ctx context.Context, l Launch) (string, error) {
	var v string
	err := c.call(ctx, "launch", request{Launch: l}, &v)
	return v, err
}
func (c *Client) Inspect(ctx context.Context, attempt string) (podman.Inspection, error) {
	var v podman.Inspection
	err := c.call(ctx, "inspect", request{Attempt: attempt}, &v)
	return v, err
}
func (c *Client) Stop(ctx context.Context, attempt string) error {
	return c.call(ctx, "stop", request{Attempt: attempt}, nil)
}
func (c *Client) Remove(ctx context.Context, attempt string) error {
	return c.call(ctx, "remove", request{Attempt: attempt}, nil)
}
func (c *Client) Release(ctx context.Context, attempt string) error {
	return c.call(ctx, "release", request{Attempt: attempt}, nil)
}
func (c *Client) Prune(ctx context.Context, keep []string, idle time.Duration) error {
	return c.call(ctx, "prune", request{Keep: keep, Idle: idle}, nil)
}

func (c *Client) EnsureImage(ctx context.Context, archive, id string) error {
	var present bool
	if err := c.call(ctx, "image", request{ID: id}, &present); err != nil || present {
		return err
	}
	return c.upload(ctx, "images/"+id, archive)
}
func (c *Client) EnsureInput(ctx context.Context, blob, digest string) error {
	var present bool
	if err := c.call(ctx, "input", request{ID: digest}, &present); err != nil || present {
		return err
	}
	return c.upload(ctx, "inputs/"+digest, blob)
}
func (c *Client) Seed(ctx context.Context, attempt, prev, archive string) error {
	var ready bool
	if err := c.call(ctx, "seed", request{Attempt: attempt, Previous: prev}, &ready); err != nil || ready {
		return err
	}
	return c.upload(ctx, "seed/"+attempt, archive)
}
func (c *Client) Collect(ctx context.Context, attempt, dir string) error {
	for _, file := range []string{StdoutFile, StderrFile} {
		f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		err = c.stream(ctx, http.MethodGet, "runs/"+attempt+"/"+file, nil, func(r io.Reader) error { _, err := io.Copy(f, r); return err })
		if err := errors.Join(err, f.Sync(), f.Close()); err != nil {
			return err
		}
	}
	outputs := filepath.Join(dir, OutputsDir)
	if err := os.Mkdir(outputs, 0o700); err != nil {
		return err
	}
	return c.stream(ctx, http.MethodGet, "runs/"+attempt+"/"+OutputsDir, nil, func(r io.Reader) error { return content.Extract(r, outputs) })
}
