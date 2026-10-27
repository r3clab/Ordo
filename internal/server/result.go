package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/worker"
)

const (
	RecordFile     = "run.json"
	OutputsArchive = "outputs.tar"
	// MetaDir is where Out keeps the job's record and each step's logs, beside the outputs.
	MetaDir = ".ordo"
	JobFile = "job.json"
)

// Record is run.json. It holds no secret values: Spec lists only their names.
type Record struct {
	Job           string          `json:"job"`
	Out           string          `json:"out"`
	Step          int             `json:"step"`
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
	if at.StopRequested == "" && j.spec().TimedOut(result.StartedAt, result.FinishedAt) {
		at.StopRequested = Timeout
	}
	dest := filepath.Join(s.Root, "results", j.ID, at.ID)
	if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
		record := Record{Job: j.ID, Out: j.Out, Step: j.Step, Attempt: at.ID, Worker: at.Worker, MachineID: at.MachineID, ContainerID: at.ContainerID, Spec: j.spec(), Allocation: at.Allocation, StopRequested: at.StopRequested, StopReason: at.StopReason, Result: result}
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
	return c.ID == at.ContainerID && c.Name == podman.Name(at.ID) && c.Image == j.spec().Image && labels[podman.AttemptLabel] == at.ID && labels[podman.ServerLabel] == s.id
}

// receive keeps the outputs only as an archive, which seeds the next step and is delivered to Out.
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
	outputs := filepath.Join(stage, worker.OutputsDir)
	record.OutputsSHA256, err = content.Archive(outputs, filepath.Join(stage, OutputsArchive))
	if err != nil {
		return err
	}
	if err := os.RemoveAll(outputs); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(stage, RecordFile), record); err != nil {
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	return content.SyncDir(parent)
}

func writeJSON(file string, value any) error {
	return content.WriteAtomic(file, func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(value)
	})
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

// deliver publishes the last attempt's outputs, with the job's record and each step's logs in MetaDir,
// into Out by one rename. A rename that a crash interrupted before the job was saved left a job.json
// naming this job, which counts as delivered.
func (s *Server) deliver(j Job) error {
	outcome := Succeeded
	if j.current().Failure != "" {
		outcome = Failed
	}
	var published Job
	if data, err := os.ReadFile(filepath.Join(j.Out, MetaDir, JobFile)); err == nil && json.Unmarshal(data, &published) == nil && published.ID == j.ID {
		return s.settle(j, outcome, "")
	}
	parent := filepath.Dir(j.Out)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".ordo-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	tree := filepath.Join(stage, "out")
	if err := unpackFile(filepath.Join(j.current().ResultDir, OutputsArchive), tree); err != nil {
		return err
	}
	meta := filepath.Join(tree, MetaDir)
	if err := os.Mkdir(meta, 0o755); errors.Is(err, os.ErrExist) {
		return s.settle(j, Failed, "outputs hold "+MetaDir+", where Ordo writes the job's record")
	} else if err != nil {
		return err
	}
	for i, at := range j.Attempts {
		if at.ResultDir == "" || (i+1 < len(j.Attempts) && j.Attempts[i+1].Step == at.Step) {
			continue
		}
		for name, ext := range map[string]string{worker.StdoutFile: ".stdout", worker.StderrFile: ".stderr"} {
			if err := copyFile(filepath.Join(at.ResultDir, name), filepath.Join(meta, j.label(at.Step)+ext)); err != nil {
				return err
			}
		}
	}
	record := j.clone()
	record.State = outcome
	if err := writeJSON(filepath.Join(meta, JobFile), record); err != nil {
		return err
	}
	// os.Rename refuses any existing directory; rename(2) replaces an empty one, such as one made ahead.
	if err := syscall.Rename(tree, j.Out); err != nil {
		return fmt.Errorf("deliver results: %w", err)
	}
	if err := content.SyncDir(parent); err != nil {
		return err
	}
	return s.settle(j, outcome, "")
}

func (s *Server) settle(j Job, outcome State, reason string) error {
	j.State, j.Error = outcome, reason
	if err := s.save(j); err != nil {
		return err
	}
	prune(j)
	return nil
}

func unpackFile(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return content.Unpack(f, dest, nil)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Sync(), out.Close())
}
