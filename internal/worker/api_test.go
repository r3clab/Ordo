package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
)

type runFunc func(context.Context, string, io.Reader, io.Writer, io.Writer) error

func (f runFunc) Run(ctx context.Context, s string, in io.Reader, out, err io.Writer) error {
	return f(ctx, s, in, out, err)
}

func apiClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "ordo-api-test-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "api")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(listener) }()
	client := socketClient(socket)
	t.Cleanup(func() { _ = client.Close(); _ = srv.Close(); _ = os.RemoveAll(dir) })
	return client
}

func apiHost(t *testing.T) *Host {
	t.Helper()
	h := testHost(t)
	h.run = process{}
	for _, dir := range []string{"runs", "images", "trees", "cdi"} {
		if err := os.MkdirAll(filepath.Join(h.StateDir, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func TestWorkerAPIExecutesAndStreamsExistingOperations(t *testing.T) {
	log := fakes(t)
	h := apiHost(t)
	c := apiClient(t, newAPI(h))
	ctx := context.Background()
	if _, err := c.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "value"), []byte("input"), 0o600); err != nil {
		t.Fatal(err)
	}
	blobs := t.TempDir()
	digest, err := content.PutTree(blobs, os.DirFS(source))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureInput(ctx, filepath.Join(blobs, digest), digest); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureInput(ctx, "/missing", digest); err != nil {
		t.Fatal("present input uploaded again", err)
	}
	if data, err := os.ReadFile(filepath.Join(h.treeDir(digest), "value")); err != nil || string(data) != "input" {
		t.Fatalf("input: %q %v", data, err)
	}
	image := "sha256:" + strings.Repeat("a", 64)
	if err := c.EnsureImage(ctx, "/missing", image); err != nil {
		t.Fatal("present image uploaded again", err)
	}
	h.forget()
	t.Setenv("ORDO_FAKE_IMAGE_EXISTS", "1")
	t.Setenv("ORDO_FAKE_IMAGE", image)
	archive := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(archive, []byte("image bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.EnsureImage(ctx, archive, image); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(log + ".loaded"); string(data) != "image bytes" {
		t.Fatalf("image: %q", data)
	}
	l := Launch{Attempt: "a1", Spec: spec.Spec{Image: image, Task: spec.Task{Resources: spec.Resources{Cores: 1, MemoryMiB: 128}}}, Allocation: pool.Allocation{CPUs: []int{0}, MemoryMiB: 128}}
	if id, err := c.Launch(ctx, l); err != nil || id != strings.Repeat("b", 64) {
		t.Fatalf("launch %q: %v", id, err)
	}
	out := filepath.Join(h.runDir("a1"), OutputsDir)
	if err := os.WriteFile(filepath.Join(out, "result"), []byte("42"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if err := c.Collect(ctx, "a1", dest); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{StdoutFile: "task stdout\n", StderrFile: "task stderr\n", "outputs/result": "42"} {
		if data, err := os.ReadFile(filepath.Join(dest, file)); err != nil || string(data) != want {
			t.Fatalf("%s: %q, %v", file, data, err)
		}
	}
	if err := c.Seed(ctx, "a2", "a1", "/missing"); err != nil {
		t.Fatal("kept outputs not moved", err)
	}
	seedArchive := filepath.Join(t.TempDir(), "outputs.tar")
	if _, err := content.Archive(filepath.Join(dest, OutputsDir), seedArchive); err != nil {
		t.Fatal(err)
	}
	if err := c.Seed(ctx, "a3", "a1", seedArchive); err != nil {
		t.Fatal("seed upload", err)
	}
	if err := c.Stop(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if err := c.Release(ctx, "a2"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "a3"); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerAPIRejectsInvalidRequestsAndIncompleteInputs(t *testing.T) {
	fakes(t)
	h := apiHost(t)
	c := apiClient(t, newAPI(h))
	ctx := context.Background()
	for _, q := range []struct {
		operation string
		q         request
	}{
		{"remove", request{Attempt: "../outside"}},
		{"input", request{ID: "../outside"}},
		{"launch", request{Launch: Launch{Attempt: "a"}}},
		{"prune", request{Idle: time.Second}},
	} {
		if err := c.call(ctx, q.operation, q.q, nil); err == nil || errors.Is(err, ssh.ErrTransport) {
			t.Fatalf("%s: %v", q.operation, err)
		}
	}
	for _, modifier := range []func(*http.Request){func(r *http.Request) { r.Host = "evil" }, func(r *http.Request) { r.Header.Set("Origin", "https://evil") }} {
		r, _ := http.NewRequest(http.MethodPost, "http://ordo-worker/rpc/poll", strings.NewReader("{}"))
		modifier(r)
		resp, err := c.http.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatal("untrusted request accepted")
		}
	}
	var tar bytes.Buffer
	if err := content.Tar(&tar, os.DirFS(t.TempDir()), false); err != nil {
		t.Fatal(err)
	}
	bad := strings.Repeat("f", 64)
	if err := c.stream(ctx, http.MethodPut, "inputs/"+bad, bytes.NewReader(tar.Bytes()), nil); err == nil || errors.Is(err, ssh.ErrTransport) {
		t.Fatalf("digest mismatch: %v", err)
	}
	if _, err := os.Stat(h.treeDir(bad)); !os.IsNotExist(err) {
		t.Fatal("bad input was published", err)
	}
	if err := c.stream(ctx, http.MethodPut, "inputs/"+bad, strings.NewReader("partial tar"), nil); err == nil {
		t.Fatal("partial input published")
	}
	if left, _ := filepath.Glob(filepath.Join(h.StateDir, "trees", ".upload-*")); len(left) != 0 {
		t.Fatal("temporary upload survived", left)
	}
}

func TestTransfersDoNotBlockControlAndQueuedCancellation(t *testing.T) {
	fakes(t)
	h := apiHost(t)
	started, release := make(chan struct{}, 3), make(chan struct{})
	h.run = runFunc(func(ctx context.Context, script string, in io.Reader, out, stderr io.Writer) error {
		if strings.Contains(script, "pm load") {
			started <- struct{}{}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
			}
		}
		return (process{}).Run(ctx, script, in, out, stderr)
	})
	c := apiClient(t, newAPI(h))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	image := "sha256:" + strings.Repeat("a", 64)
	t.Setenv("ORDO_FAKE_IMAGE", image)
	done := make(chan error, 2)
	for range 2 {
		go func() { done <- c.stream(ctx, http.MethodPut, "images/"+image, strings.NewReader("archive"), nil) }()
	}
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("transfers did not start")
		}
	}
	if _, err := c.Poll(ctx); err != nil {
		t.Fatal("transfer blocked control", err)
	}
	queued, stop := context.WithCancel(ctx)
	stopped := make(chan error, 1)
	go func() {
		stopped <- c.stream(queued, http.MethodPut, "images/"+image, strings.NewReader("archive"), nil)
	}()
	stop()
	if err := <-stopped; !errors.Is(err, context.Canceled) {
		t.Fatalf("queued cancellation: %v", err)
	}
	close(release)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-started:
		t.Fatal("cancelled transfer ran")
	default:
	}
}

func TestStreamRequiresCompletionAndKeepsRemoteErrors(t *testing.T) {
	for _, tc := range []struct {
		name, trailer string
		transport     bool
		code          int
	}{
		{"missing", "", true, 0},
		{"corrupt", "bad", true, 0},
		{"remote", base64.StdEncoding.EncodeToString([]byte(`{"error":"failed","exit":255}`)), false, 255},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := apiClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Trailer", resultTrailer)
				_, _ = io.WriteString(w, "partial")
				w.Header().Set(resultTrailer, tc.trailer)
			}))
			err := c.stream(context.Background(), http.MethodGet, "runs/a1/outputs", nil, nil)
			if err == nil || errors.Is(err, ssh.ErrTransport) != tc.transport {
				t.Fatalf("stream: %v", err)
			}
			if tc.code != 0 {
				var exit *ssh.ExitError
				if !errors.As(err, &exit) || exit.Code != tc.code {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestHelperLeaseClosesSocketAndCancelsRequests(t *testing.T) {
	fakes(t)
	h := apiHost(t)
	in, lease := io.Pipe()
	out, ready := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), in, ready); _ = ready.Close() }()
	if err := json.NewEncoder(lease).Encode(initialization{Protocol: protocol, Config: h.Config, Server: "s1"}); err != nil {
		t.Fatal(err)
	}
	var socket string
	buf := make([]byte, 1)
	for {
		if _, err := out.Read(buf); err != nil {
			t.Fatal(err)
		}
		if buf[0] == '\n' {
			break
		}
		socket += string(buf)
	}
	client := socketClient(socket)
	defer func() { _ = client.Close() }()
	if info, err := os.Stat(socket); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket: %v %v", info, err)
	}
	if _, err := client.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper survived lease EOF")
	}
	if _, err := os.Stat(filepath.Dir(socket)); !os.IsNotExist(err) {
		t.Fatal("helper socket directory survived", err)
	}
	if _, err := client.Poll(context.Background()); !errors.Is(err, ssh.ErrTransport) {
		t.Fatalf("closed helper: %v", err)
	}
	_ = in.Close()
	_ = out.Close()
}

type brokenWriter struct{ err error }

func (w brokenWriter) Write([]byte) (int, error) { return 0, w.err }

func TestHelperRejectsPublicStateBeforePreparingServices(t *testing.T) {
	h := apiHost(t)
	if err := os.Chmod(h.StateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(initialization{Protocol: protocol, Config: h.Config, Server: "s1", Services: map[string]string{"test": "127.0.0.1:1"}})
	if err := Serve(context.Background(), strings.NewReader(string(data)+"\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "700") {
		t.Fatalf("public state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.StateDir, "services")); !os.IsNotExist(err) {
		t.Fatalf("service setup touched unprotected state: %v", err)
	}
}

func TestHelperReportsReadinessWriteFailure(t *testing.T) {
	h := apiHost(t)
	data, _ := json.Marshal(initialization{Protocol: protocol, Config: h.Config, Server: "s1"})
	broken := errors.New("readiness pipe closed")
	if err := Serve(context.Background(), strings.NewReader(string(data)+"\n"), brokenWriter{broken}); !errors.Is(err, broken) {
		t.Fatalf("readiness failure: %v", err)
	}
}

func TestLocalProcessReportsOutputWriteFailure(t *testing.T) {
	broken := errors.New("disk full")
	if err := (process{}).Run(context.Background(), "yes | head -c 1048576", nil, brokenWriter{broken}, nil); !errors.Is(err, broken) {
		t.Fatalf("write failure: %v", err)
	}
}

func TestLocalProcessPreservesExitStatusAndCancellation(t *testing.T) {
	var out bytes.Buffer
	if err := (process{}).Run(context.Background(), "cat; echo problem >&2; exit 255", strings.NewReader("input"), &out, nil); err == nil || errors.Is(err, ssh.ErrTransport) {
		t.Fatalf("command error: %v", err)
	} else {
		var exit *ssh.ExitError
		if !errors.As(err, &exit) || exit.Code != 255 || exit.Stderr != "problem" || out.String() != "input" {
			t.Fatalf("exit: %v, %q", err, out.String())
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := (process{}).Run(ctx, "sleep 30", nil, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
