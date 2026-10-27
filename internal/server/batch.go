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
	"slices"
)

const maxBatch = 10000

// A batch commits in chunks: one transaction per job would fsync per line, and one for the whole
// batch would report nothing until the last job is prepared.
const batchChunk = 256

type BatchEntry struct {
	Line     int
	Request  Request
	Workflow *Workflow
	Err      error
	steps    []Step
}

type BatchResult struct {
	ID       string `json:"id,omitempty"`
	Line     int    `json:"line,omitempty"`
	Workflow bool   `json:"workflow,omitempty"`
	State    State  `json:"state,omitempty"`
	Existing bool   `json:"existing,omitempty"`
	Error    string `json:"error,omitempty"`
}

func ParseBatch(r io.Reader) ([]BatchEntry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, 8<<20)
	var entries []BatchEntry
	for line := 1; scanner.Scan(); line++ {
		data := bytes.TrimSpace(scanner.Bytes())
		if len(data) == 0 {
			continue
		}
		if len(entries) == maxBatch {
			return nil, fmt.Errorf("batch exceeds %d lines", maxBatch)
		}
		e := BatchEntry{Line: line}
		var probe struct {
			Tasks json.RawMessage `json:"tasks"`
		}
		if json.Unmarshal(data, &probe) == nil && probe.Tasks != nil {
			e.Workflow = &Workflow{}
			e.Err = DecodeJSON(bytes.NewReader(data), e.Workflow)
		} else {
			e.Err = DecodeJSON(bytes.NewReader(data), &e.Request)
			if e.Err == nil && e.Request.ID == "" {
				e.Err = errors.New("batch jobs need an id so resubmission stays idempotent")
			}
		}
		entries = append(entries, e)
	}
	return entries, scanner.Err()
}

func (s *Server) SubmitBatch(ctx context.Context, entries []BatchEntry) iter.Seq[BatchResult] {
	return func(yield func(BatchResult) bool) {
		var requests []Request
		for i := range entries {
			e := &entries[i]
			if e.Err != nil {
				continue
			}
			if e.Workflow == nil {
				if e.Err = s.check(e.Request); e.Err == nil {
					requests = append(requests, e.Request)
				}
				continue
			}
			if e.steps, e.Err = s.checkWorkflow(*e.Workflow); e.Err == nil {
				for _, step := range e.steps {
					requests = append(requests, step.Request)
				}
			}
		}
		st := s.stage(ctx, requests...)
		for chunk := range slices.Chunk(entries, batchChunk) {
			results := make([]BatchResult, len(chunk))
			var jobs []Job
			var slots []int
			for i, e := range chunk {
				results[i] = BatchResult{ID: e.Request.ID, Line: e.Line}
				err := cmp.Or(e.Err, ctx.Err())
				if e.Workflow != nil {
					results[i].ID, results[i].Workflow = e.Workflow.ID, true
					if err == nil {
						results[i].Existing, err = s.admitWorkflow(e.Workflow.ID, e.steps, st)
					}
					if err != nil {
						results[i].Error = err.Error()
					}
					continue
				}
				var j Job
				if err == nil {
					j, err = s.prepare(e.Request, nil, st)
				}
				if err != nil {
					results[i].Error = err.Error()
					continue
				}
				jobs = append(jobs, j)
				slots = append(slots, i)
			}
			for k, admitted := range s.admit(jobs) {
				result := &results[slots[k]]
				if admitted.err != nil {
					result.Error = admitted.err.Error()
				} else {
					result.State, result.Existing = admitted.state, admitted.existing
				}
			}
			for _, result := range results {
				if !yield(result) {
					return
				}
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
	for result := range s.SubmitBatch(r.Context(), entries) {
		if encoder.Encode(result) != nil || flush.Flush() != nil {
			return
		}
	}
}
