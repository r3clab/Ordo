package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/worker"
)

const (
	RecordFile     = "run.json"
	OutputsArchive = "outputs.tar"
)

var resultFiles = []string{RecordFile, worker.StdoutFile, worker.StderrFile}

// Record is run.json. It holds no secret values: Spec lists only their names.
type Record struct {
	Job           string          `json:"job"`
	Attempt       string          `json:"attempt"`
	Worker        string          `json:"worker"`
	MachineID     string          `json:"machine_id"`
	ContainerID   string          `json:"container_id"`
	Spec          spec.Spec       `json:"spec"`
	Allocation    pool.Allocation `json:"allocation"`
	StopRequested StopKind        `json:"stop_requested,omitempty"`
	StopReason    string          `json:"stop_reason,omitempty"`
	Result
}

// collect verifies the exited container is the attempt's, then publishes its logs and outputs as one
// result directory before anything marks the attempt finished.
func (s *Server) collect(ctx context.Context, j Job, v *view) error {
	at := j.current()
	if j.State != Collecting {
		j.State = Collecting
		if err := s.save(j); err != nil {
			return err
		}
	}
	c, err := v.host.Inspect(ctx, at.ID)
	if err != nil {
		return err
	}
	if !s.owns(j, c) {
		return s.block(j, "exited container does not match its attempt")
	}
	state := c.State
	if state.Status != podman.Exited || state.ExitCode == nil || *state.ExitCode < 0 || *state.ExitCode > 255 {
		return s.block(j, "exited container has no valid exit code")
	}
	// Podman cannot record an exit that a reboot interrupted.
	if state.FinishedAt.IsZero() || state.FinishedAt.Before(state.StartedAt) {
		return s.abandon(ctx, j, v, "container ended without a recorded finish time")
	}
	result := Result{ExitCode: *state.ExitCode, StartedAt: state.StartedAt, FinishedAt: state.FinishedAt, OOMKilled: state.OOMKilled}
	if at.StopRequested == "" && j.Spec.TimedOut(result.StartedAt, result.FinishedAt) {
		at.StopRequested = Timeout
	}
	dest := filepath.Join(s.Root, "results", j.ID, at.ID)
	if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
		record := Record{Job: j.ID, Attempt: at.ID, Worker: at.Worker, MachineID: at.MachineID, ContainerID: at.ContainerID, Spec: j.Spec, Allocation: at.Allocation, StopRequested: at.StopRequested, StopReason: at.StopReason, Result: result}
		err := s.receive(ctx, v.host, &record, dest)
		if end := fatal(ctx, err); end != nil {
			return end
		}
		if err != nil {
			return s.block(j, "collect results: "+err.Error())
		}
	} else if err != nil {
		return err
	}
	saved, err := readRecord(dest)
	if err != nil {
		return err
	}
	if saved.Attempt != at.ID || saved.ContainerID != at.ContainerID || saved.ExitCode != result.ExitCode {
		return s.block(j, "saved result does not match its attempt")
	}
	at.Result = &saved.Result
	at.ResultDir = dest
	if err := s.finish(j, failure(*at)); err != nil {
		return err
	}
	_ = v.host.Remove(ctx, at.ID)
	return nil
}

func (s *Server) owns(j Job, c podman.Inspection) bool {
	at := j.current()
	labels := c.Config.Labels
	return c.ID == at.ContainerID && c.Name == podman.Name(at.ID) && c.Image == j.Spec.Image && labels[podman.AttemptLabel] == at.ID && labels[podman.ServerLabel] == s.id
}

func (s *Server) receive(ctx context.Context, h host, record *Record, dest string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".collect-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := h.Collect(ctx, record.Attempt, stage); err != nil {
		return err
	}
	// Workflow inputs reuse this archive by digest, so it is rebuilt deterministically on the server.
	record.OutputsSHA256, err = content.Archive(filepath.Join(stage, worker.OutputsDir), filepath.Join(stage, OutputsArchive))
	if err != nil {
		return err
	}
	err = content.WriteAtomic(filepath.Join(stage, RecordFile), func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(record)
	})
	if err != nil {
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	return content.SyncDir(parent)
}

func readRecord(dir string) (Record, error) {
	var r Record
	data, err := os.ReadFile(filepath.Join(dir, RecordFile))
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(data, &r)
}

// failure counts a timeout against the task, although the server or conmon is what stopped it.
func failure(at Attempt) Failure {
	switch {
	case at.StopRequested == Timeout:
		return TaskFailure
	case at.StopRequested == Cancel:
		return CancelFailure
	case at.Result == nil:
		return InfrastructureFailure
	case at.Result.ExitCode != 0:
		return TaskFailure
	}
	return ""
}

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
	outputs := filepath.Join(dir, worker.OutputsDir)
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
	serveTar(w, parts...)
}
