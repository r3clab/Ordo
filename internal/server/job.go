package server

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/spec"
)

type RetryPolicy struct {
	Task           int  `json:"task,omitempty"`
	Infrastructure *int `json:"infrastructure,omitempty"`
}

const (
	MaxRetry = 10
	MaxSteps = 32
)

func (r RetryPolicy) validate() error {
	if r.Task < 0 || r.Task > MaxRetry || (r.Infrastructure != nil && (*r.Infrastructure < 0 || *r.Infrastructure > MaxRetry)) {
		return fmt.Errorf("retry counts must be 0..%d", MaxRetry)
	}
	return nil
}

func (r RetryPolicy) limit(class Failure) int {
	switch {
	case class == TaskFailure:
		return r.Task
	case r.Infrastructure == nil:
		return 1
	}
	return *r.Infrastructure
}

func jobKey(j Job) string {
	type step struct {
		Name string
		Spec spec.Spec
	}
	steps := make([]step, len(j.Steps))
	for i, stage := range j.Steps {
		// Inputs count by source, so a resubmission matches once a waiting input has been snapshotted.
		stage.Spec.Inputs = nil
		steps[i] = step{stage.Name, stage.Spec}
	}
	data, _ := json.Marshal(struct {
		Out    string
		Steps  []step
		Inputs map[string]string
		Retry  RetryPolicy
	}{j.Out, steps, j.Inputs, j.Retry})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

// label names a step's files in Out: its name, or its position counted from one.
func (j Job) label(step int) string {
	return cmp.Or(j.Steps[step].Name, strconv.Itoa(step+1))
}

// checkpoint is the attempt whose outputs the current step starts from, nil for the first step.
func (j Job) checkpoint() *Attempt {
	for i := len(j.Attempts) - 1; i >= 0; i-- {
		if j.Attempts[i].Step == j.Step-1 {
			return &j.Attempts[i]
		}
	}
	return nil
}

func (s *Server) Cancel(id, reason string) error {
	return s.requestStop(id, reason, func(j *Job) { j.CancelRequested = true })
}

func (s *Server) Stop(id, reason string) error {
	return s.requestStop(id, reason, func(j *Job) { j.StopRequested = true })
}

// requestStop leaves a delivering job alone, as its last step has already ended.
func (s *Server) requestStop(id, reason string, mark func(*Job)) error {
	s.write.Lock()
	defer s.write.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.State == Delivering {
		return nil
	}
	j = j.clone()
	s.mu.Lock()
	mark(&j)
	j.StopReason = cmp.Or(j.StopReason, reason)
	// A pass that took a stale copy cannot revive the job, as save drops writes to finished jobs.
	if j.State == Queued && !s.busy[j.ID] {
		j.State = Cancelled
	}
	s.mu.Unlock()
	return s.commit(j)
}

// finish settles the current attempt. A step that succeeds hands the job to the next one; the last
// step's outcome, success or the task's own failure, is delivered to Out. A stop ends only the step it
// reaches, so the later steps still run.
func (s *Server) finish(j Job, class Failure) error {
	s.refresh(&j)
	at := j.current()
	at.Failure = class
	count := 0
	for _, a := range j.Attempts {
		if a.Step == j.Step && a.Failure == class {
			count++
		}
	}
	exhausted := j.StopRequested || count > j.Retry.limit(class)
	switch {
	case j.CancelRequested || class == CancelFailure:
		j.State = Cancelled
	case class == "" && j.Step+1 < len(j.Steps):
		j.Step++
		j.State, j.Error, j.ReadyAt = Queued, "", time.Now().UTC()
		j.StopRequested, j.StopReason = false, ""
	case class == "" || (exhausted && at.Result != nil):
		j.State = Delivering
	case exhausted:
		j.State = Failed
	default:
		j.requeue(backoff(count), at.Worker)
	}
	return s.save(j)
}

func (j *Job) requeue(delay time.Duration, avoid string) {
	j.State, j.ReadyAt, j.Avoid = Queued, time.Now().UTC().Add(delay), avoid
}

// prune drops a finished job's output archives; Out holds what is kept of them.
func prune(j Job) {
	for _, at := range j.Attempts {
		if at.ResultDir != "" {
			_ = os.Remove(filepath.Join(at.ResultDir, OutputsArchive))
		}
	}
}

// kept is the attempt whose run directory still holds, on its worker, the outputs the current step is
// to start from: the step's launch moves them from there.
func (j Job) kept() string {
	if prev := j.checkpoint(); prev != nil && (j.State == Queued || j.State == Dispatching) {
		return prev.ID
	}
	return ""
}

// await holds a job until the jobs it takes inputs from have finished: it is skipped when one of them
// did not succeed, and its waiting inputs are snapshotted from their Out once all did.
func (s *Server) await(j Job, v *view) error {
	s.mu.Lock()
	var states []State
	for _, id := range j.After {
		states = append(states, s.records[id].State)
	}
	s.mu.Unlock()
	for _, state := range states {
		if state.Terminal() && state != Succeeded {
			j.State, j.Error = Skipped, "a job it takes inputs from did not succeed"
			return s.save(j)
		}
	}
	if slices.ContainsFunc(states, func(state State) bool { return state != Succeeded }) {
		return nil
	}
	return v.later(func() error { return s.snapshot(j) })
}

func (s *Server) snapshot(j Job) error {
	s.staging.RLock()
	defer s.staging.RUnlock()
	inputs := map[string]string{}
	for _, name := range j.Waiting {
		digest, err := content.PutTree(s.images.Blobs, j.Inputs[name])
		if err != nil {
			j.State, j.Error = Failed, fmt.Sprintf("input %s: %v", name, err)
			return s.save(j)
		}
		inputs[name] = digest
	}
	j.Steps = slices.Clone(j.Steps)
	for i := range j.Steps {
		j.Steps[i].Spec.Inputs = maps.Clone(j.Steps[i].Spec.Inputs)
		if j.Steps[i].Spec.Inputs == nil {
			j.Steps[i].Spec.Inputs = map[string]string{}
		}
		maps.Copy(j.Steps[i].Spec.Inputs, inputs)
	}
	j.Waiting = nil
	return s.save(j)
}
