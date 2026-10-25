package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

func (a *Agent) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/info", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		info := a.info
		for _, run := range a.attempts {
			if !run.Terminal() {
				info.Held = append(info.Held, run.Allocation)
			}
		}
		a.mu.Unlock()
		respond(w, http.StatusOK, info)
	})
	mux.HandleFunc("PUT /v1/attempts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var spec Spec
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&spec); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			http.Error(w, "expected one JSON request", http.StatusBadRequest)
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
			case errors.Is(err, ErrCapacity), errors.Is(err, ErrUnavailable), errors.Is(err, context.Canceled):
				status = http.StatusServiceUnavailable
			}
			http.Error(w, err.Error(), status)
			return
		}
		respond(w, http.StatusOK, run)
	})
	mux.HandleFunc("GET /v1/attempts/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		run, ok := a.attempts[r.PathValue("id")]
		a.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		respond(w, http.StatusOK, run)
	})
	mux.HandleFunc("GET /v1/attempts", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		runs := make([]Attempt, 0, len(a.attempts))
		for _, run := range a.attempts {
			runs = append(runs, run)
		}
		a.mu.Unlock()
		respond(w, http.StatusOK, runs)
	})
	return mux
}

func respond(w http.ResponseWriter, status int, value any) {
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

func Socket(stateDir string) string { return filepath.Join(stateDir, "agent.sock") }
