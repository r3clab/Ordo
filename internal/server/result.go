package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/task"
	"github.com/r3clab/Ordo/internal/worker"
)

var resultFiles = []string{task.RecordFile, task.StdoutFile, task.StderrFile}

func (j Job) resultDir(index int) (string, error) {
	if index < 0 || index >= len(j.Attempts) || j.Attempts[index].ResultDir == "" {
		return "", errors.New("results are available after collection")
	}
	return j.Attempts[index].ResultDir, nil
}

func (s *Server) serveResult(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	dir, err := j.resultDir(len(j.Attempts) - 1)
	if j.State != Succeeded || err != nil {
		http.Error(w, "job has not succeeded", http.StatusConflict)
		return
	}
	outputs := filepath.Join(dir, task.OutputsDir)
	entries, err := os.ReadDir(outputs)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	for _, entry := range entries {
		if slices.Contains(resultFiles, entry.Name()) {
			http.Error(w, fmt.Sprintf("output %s collides with a result file", entry.Name()), http.StatusConflict)
			return
		}
	}
	parts := []content.Part{{Path: outputs}}
	for _, name := range resultFiles {
		parts = append(parts, content.Part{Path: filepath.Join(dir, name), Name: name})
	}
	worker.ServeTar(w, parts...)
}
