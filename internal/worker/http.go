package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (a *Agent) handler() http.Handler {
	mux := http.NewServeMux()
	a.contentRoutes(mux)
	mux.HandleFunc("POST /v1/attempts/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if err := a.Cancel(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]bool{"requested": true})
	})
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		info := a.info
		info.Held = a.held()
		a.mu.Unlock()
		WriteJSON(w, http.StatusOK, info)
	})
	mux.HandleFunc("PUT /v1/attempts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var spec Spec
		if err := DecodeJSON(http.MaxBytesReader(w, r.Body, 1<<20), &spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		run, err := a.Submit(r.PathValue("id"), spec)
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
		runs := make([]Attempt, 0, len(a.attempts))
		for _, run := range a.attempts {
			runs = append(runs, run)
		}
		a.mu.Unlock()
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
