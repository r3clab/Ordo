package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func (a *Agent) handler() http.Handler {
	mux := http.NewServeMux()
	a.contentRoutes(mux)
	mux.HandleFunc("POST /v1/attempts/{id}/cancel", StopHandler(a.Cancel, http.StatusConflict))
	mux.HandleFunc("POST /v1/attempts/{id}/stop", StopHandler(a.Stop, http.StatusConflict))
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		info := a.info
		info.Held = a.held()
		a.mu.Unlock()
		WriteJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("PUT /v1/attempts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var submission Submission
		if err := DecodeJSON(http.MaxBytesReader(w, r.Body, 1<<20), &submission); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		run, err := a.Submit(r.PathValue("id"), submission.Spec, submission.Secrets)
		if err != nil {
			status := http.StatusInternalServerError
			switch {
			case errors.Is(err, ErrInvalidRequest):
				status = http.StatusBadRequest
			case errors.Is(err, ErrConflict):
				status = http.StatusConflict
			case errors.Is(err, ErrCapacity):
				status = http.StatusTooManyRequests
			case errors.Is(err, ErrUnavailable), errors.Is(err, context.Canceled):
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		WriteJSON(w, http.StatusOK, run)
	})
	mux.HandleFunc("GET /v1/attempts/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		run, ok := a.attempts[r.PathValue("id")]
		a.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		WriteJSON(w, http.StatusOK, run)
	})
	mux.HandleFunc("GET /v1/attempts", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		runs := slices.AppendSeq(make([]Attempt, 0, len(a.attempts)), maps.Values(a.attempts))
		a.mu.Unlock()
		slices.SortFunc(runs, func(x, y Attempt) int { return x.CreatedAt.Compare(y.CreatedAt) })
		WriteJSON(w, http.StatusOK, runs)
	})
	return mux
}

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

type StopRequest struct {
	Reason string `json:"reason,omitempty"`
}

func StopHandler(request func(id, reason string) error, failure int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reason, err := DecodeReason(w, r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := request(r.PathValue("id"), reason); err != nil {
			http.Error(w, err.Error(), failure)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]bool{"requested": true})
	}
}

func StopAttempt(ctx context.Context, client *http.Client, id string, kind StopKind, reason string) error {
	return Call(ctx, client, http.MethodPost, "/v1/attempts/"+url.PathEscape(id)+"/"+string(kind), StopRequest{Reason: reason}, nil)
}

// DecodeReason accepts an empty body, which the web UI sends to cancel without a reason.
func DecodeReason(w http.ResponseWriter, r *http.Request) (string, error) {
	// JSON escapes can take six bytes per character of the reason.
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8*MaxReasonBytes))
	if err != nil || len(data) == 0 {
		return "", err
	}
	var request StopRequest
	if err := DecodeJSON(bytes.NewReader(data), &request); err != nil {
		return "", err
	}
	if len(request.Reason) > MaxReasonBytes {
		return "", fmt.Errorf("reason exceeds %d bytes", MaxReasonBytes)
	}
	return request.Reason, nil
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

func Client(socket string) *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}

func untimed(client *http.Client) *http.Client {
	c := *client
	c.Timeout = 0
	return &c
}

func checkResponse(response *http.Response) error {
	if response.StatusCode/100 == 2 {
		return nil
	}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	return &HTTPError{Status: response.StatusCode, Message: strings.TrimSpace(string(data))}
}

func Socket(stateDir string) string { return filepath.Join(stateDir, "agent.sock") }

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
