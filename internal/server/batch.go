package server

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"slices"

	"github.com/r3clab/Ordo/internal/image"
)

const maxBatch = 10000

// MaxBatchLine bounds one line of a batch, which the CLI reads before the server does.
const MaxBatchLine = 8 << 20

// A batch commits in chunks: one transaction per job would fsync per line, and one for the whole
// batch would report nothing until the last job is prepared.
const batchChunk = 256

type BatchEntry struct {
	Line    int
	Request Request
	Err     error
}

// BatchResult reports one line: the job it queued or found, or Done when Out already holds results.
type BatchResult struct {
	Line     int    `json:"line,omitempty"`
	Out      string `json:"out,omitempty"`
	ID       string `json:"id,omitempty"`
	State    State  `json:"state,omitempty"`
	Existing bool   `json:"existing,omitempty"`
	Done     bool   `json:"done,omitempty"`
	Error    string `json:"error,omitempty"`
}

func ParseBatch(r io.Reader) ([]BatchEntry, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(nil, MaxBatchLine)
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
		e.Err = DecodeJSON(bytes.NewReader(data), &e.Request)
		entries = append(entries, e)
	}
	return entries, scanner.Err()
}

// SubmitBatch builds no image for a line whose Out already holds results. An input may lie in the Out
// of an unfinished job or an earlier line's, and then waits for that job.
func (s *Server) SubmitBatch(ctx context.Context, entries []BatchEntry) iter.Seq[BatchResult] {
	return func(yield func(BatchResult) bool) {
		s.staging.RLock()
		defer s.staging.RUnlock()
		type line struct {
			BatchEntry
			done    bool
			waiting []string
		}
		lines := make([]line, len(entries))
		var images []image.Source
		var inputs []string
		outs := map[string]bool{}
		for i := range lines {
			e := &lines[i]
			e.BatchEntry = entries[i]
			if e.Err == nil {
				e.Err = s.check(e.Request)
			}
			if e.Err == nil {
				e.done, e.Err = s.delivered(e.Request.Out)
			}
			if e.Err == nil && !e.done {
				e.waiting = s.waiting(e.Request, outs)
				for _, step := range e.Request.steps() {
					images = append(images, step.Image)
				}
				for name, source := range e.Request.Inputs {
					if !slices.Contains(e.waiting, name) {
						inputs = append(inputs, source)
					}
				}
				outs[e.Request.Out] = true
			}
		}
		st := s.stage(ctx, images, inputs)
		for chunk := range slices.Chunk(lines, batchChunk) {
			results := make([]BatchResult, len(chunk))
			var jobs []Job
			var slots []*BatchResult
			for i, e := range chunk {
				results[i] = BatchResult{Line: e.Line, Out: e.Request.Out, Done: e.done}
				if e.done {
					continue
				}
				err := cmp.Or(e.Err, ctx.Err())
				var j Job
				if err == nil {
					j, err = s.prepare(e.Request, e.waiting, st)
				}
				if err != nil {
					results[i].Error = err.Error()
					continue
				}
				jobs = append(jobs, j)
				slots = append(slots, &results[i])
			}
			s.admit(jobs, slots)
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
