package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/worker"
)

var resultFiles = []string{"run.json", "stdout.log", "stderr.log"}

func (j Job) resultDir(index int) (string, error) {
	if index < 0 || index >= len(j.Attempts) || j.Attempts[index].ResultDir == "" {
		return "", errors.New("results are available after collection")
	}
	return j.Attempts[index].ResultDir, nil
}

// The archive streams with its hash in a trailer, so it needs no staging copy.
func (s *Server) serveResult(w http.ResponseWriter, r *http.Request) {
	j, ok := s.job(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	dir, err := j.resultDir(len(j.Attempts) - 1)
	if j.State != "succeeded" || err != nil {
		http.Error(w, "job has not succeeded", http.StatusConflict)
		return
	}
	outputs := filepath.Join(dir, "outputs")
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
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Trailer", worker.ChecksumHeader)
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
	w.Header().Set(worker.ChecksumHeader, hex.EncodeToString(hash.Sum(nil)))
}

type sentWriter struct {
	io.Writer
	sent bool
}

func (w *sentWriter) Write(p []byte) (int, error) {
	w.sent = w.sent || len(p) > 0
	return w.Writer.Write(p)
}
