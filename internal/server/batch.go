package server

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"

	"github.com/r3clab/Ordo/internal/worker"
)

const maxBatch = 10000

type BatchEntry struct {
	Line    int
	Request Request
	Err     error
}

type BatchResult struct {
	ID       string `json:"id,omitempty"`
	Line     int    `json:"line,omitempty"`
	State    string `json:"state,omitempty"`
	Existing bool   `json:"existing,omitempty"`
	Error    string `json:"error,omitempty"`
}

func ParseBatch(r io.Reader) ([]BatchEntry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 1<<20)
	var entries []BatchEntry
	for line := 1; scanner.Scan(); line++ {
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		if len(entries) == maxBatch {
			return nil, fmt.Errorf("batch exceeds %d jobs", maxBatch)
		}
		e := BatchEntry{Line: line}
		e.Err = worker.DecodeJSON(bytes.NewReader(data), &e.Request)
		if e.Err == nil && e.Request.ID == "" {
			e.Err = errors.New("batch jobs need an id so resubmission stays idempotent")
		}
		entries = append(entries, e)
	}
	return entries, scanner.Err()
}

func (s *Server) SubmitBatch(ctx context.Context, caller Caller, entries []BatchEntry) iter.Seq[BatchResult] {
	return func(yield func(BatchResult) bool) {
		var requests []Request
		for _, e := range entries {
			if e.Err == nil {
				requests = append(requests, e.Request)
			}
		}
		st := s.stage(ctx, caller, requests...)
		for _, e := range entries {
			result := BatchResult{ID: e.Request.ID, Line: e.Line}
			err := cmp.Or(e.Err, ctx.Err())
			if err == nil {
				var j Job
				if j, err = s.prepare(ctx, e.Request, st); err == nil {
					j, result.Existing, err = s.admit(j, e.Request.Secrets)
				}
				if err == nil {
					result.State = j.State
				}
			}
			if err != nil {
				result.Error = err.Error()
			}
			if !yield(result) {
				return
			}
		}
	}
}

func (s *Server) serveBatch(w http.ResponseWriter, r *http.Request) {
	entries, err := ParseBatch(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	flush := http.NewResponseController(w)
	encoder := json.NewEncoder(w)
	for result := range s.SubmitBatch(r.Context(), Local, entries) {
		if encoder.Encode(result) != nil || flush.Flush() != nil {
			return
		}
	}
}
