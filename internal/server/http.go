package server

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/r3clab/Ordo/internal/content"
)

const ChecksumHeader = "X-Ordo-SHA256"

const maxReasonBytes = 1024

type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func DecodeJSON(r io.Reader, value any) error {
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected exactly one JSON value")
	}
	return nil
}

func WriteJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

type StopRequest struct {
	Reason string `json:"reason,omitempty"`
}

func StopHandler(request func(id, reason string) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reason, err := decodeReason(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := request(r.PathValue("id"), reason); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]bool{"requested": true})
	}
}

// decodeReason accepts an empty body, which the web UI sends to cancel without a reason.
func decodeReason(w http.ResponseWriter, r *http.Request) (string, error) {
	// JSON escapes can take six bytes per character of the reason.
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8*maxReasonBytes))
	if err != nil || len(data) == 0 {
		return "", err
	}
	var request StopRequest
	if err := DecodeJSON(bytes.NewReader(data), &request); err != nil {
		return "", err
	}
	if len(request.Reason) > maxReasonBytes {
		return "", fmt.Errorf("reason exceeds %d bytes", maxReasonBytes)
	}
	return request.Reason, nil
}

// Client sets no timeout: the queue API is a local socket, and nix builds behind a submit can take
// minutes.
func Client(socket string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}

func Call(ctx context.Context, client *http.Client, method, endpoint string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	response, err := Send(ctx, client, method, endpoint, "application/json", body)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			return err
		}
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func Send(ctx context.Context, client *http.Client, method, endpoint, contentType string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://ordo"+endpoint, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return nil, errors.Join(&HTTPError{Status: response.StatusCode, Message: strings.TrimSpace(string(data))}, response.Body.Close())
	}
	return response, nil
}

func ListenUnix(socket string) (net.Listener, error) {
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s is not a socket", socket)
		}
		if conn, err := net.DialTimeout("unix", socket, time.Second); err == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("%s is already live", socket)
		}
		if err := os.Remove(socket); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		return nil, errors.Join(err, listener.Close())
	}
	return listener, nil
}

// serveTar streams parts with their digest in a trailer, so the archive needs no staging copy.
func serveTar(w http.ResponseWriter, parts ...content.Part) {
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Trailer", ChecksumHeader)
	hash := sha256.New()
	body := &sentWriter{Writer: w}
	if err := content.Tar(io.MultiWriter(body, hash), parts...); err != nil {
		if body.sent {
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Trailer")
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set(ChecksumHeader, hex.EncodeToString(hash.Sum(nil)))
}

type sentWriter struct {
	io.Writer
	sent bool
}

func (w *sentWriter) Write(p []byte) (int, error) {
	w.sent = w.sent || len(p) > 0
	return w.Writer.Write(p)
}

func Download(ctx context.Context, client *http.Client, endpoint, dest string) error {
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("result destination: %w", fs.ErrExist)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	resp, err := Send(ctx, client, http.MethodGet, endpoint, "", nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return content.Unpack(resp.Body, dest, func(digest string) error {
		if want := cmp.Or(resp.Header.Get(ChecksumHeader), resp.Trailer.Get(ChecksumHeader)); !content.Valid(want) {
			return errors.New("result missing checksum")
		} else if want != digest {
			return errors.New("content checksum mismatch")
		}
		return nil
	})
}
