package server

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"slices"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/spec"
)

type RetryPolicy struct {
	Task           int  `json:"task,omitempty"`
	Infrastructure *int `json:"infrastructure,omitempty"`
}

const (
	MaxRetry         = 10
	MaxWorkflowID    = 24
	MaxStepID        = 32
	MaxWorkflowTasks = 1000
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

type Workflow struct {
	ID    string `json:"id"`
	Tasks []Step `json:"tasks"`
}
type Step struct {
	Request
	Depends []string          `json:"depends_on,omitempty"`
	From    map[string]string `json:"from,omitempty"`
}

func (w Workflow) scope() ([]Step, error) {
	if !spec.ValidName(w.ID) || len(w.ID) > MaxWorkflowID || len(w.Tasks) == 0 || len(w.Tasks) > MaxWorkflowTasks {
		return nil, fmt.Errorf("workflow needs an ID (1..%d characters) and 1..%d tasks", MaxWorkflowID, MaxWorkflowTasks)
	}
	graph := map[string][]string{}
	for _, step := range w.Tasks {
		if _, dup := graph[step.ID]; dup || !spec.ValidName(step.ID) || len(step.ID) > MaxStepID {
			return nil, fmt.Errorf("task IDs must be unique and 1..%d characters", MaxStepID)
		}
		graph[step.ID] = nil
	}
	scoped := func(id string) string { return w.ID + "--" + id }
	steps := make([]Step, len(w.Tasks))
	for i, step := range w.Tasks {
		deps := slices.Clone(step.Depends)
		step.From = maps.Clone(step.From)
		for name, from := range step.From {
			if !spec.ValidName(name) {
				return nil, errors.New("invalid dependency input name")
			}
			if _, ok := step.Inputs[name]; ok {
				return nil, errors.New("input has both local and dependency sources")
			}
			deps = append(deps, from)
			step.From[name] = scoped(from)
		}
		slices.Sort(deps)
		deps = slices.Compact(deps)
		step.Depends = nil
		for _, dep := range deps {
			if _, ok := graph[dep]; !ok {
				return nil, fmt.Errorf("unknown dependency %s", dep)
			}
			step.Depends = append(step.Depends, scoped(dep))
		}
		graph[step.ID] = deps
		step.ID = scoped(step.ID)
		steps[i] = step
	}
	return steps, acyclic(graph)
}

func acyclic(graph map[string][]string) error {
	const visiting, done = 1, 2
	visited := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		switch visited[id] {
		case visiting:
			return errors.New("workflow dependency cycle")
		case done:
			return nil
		}
		visited[id] = visiting
		for _, dep := range graph[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[id] = done
		return nil
	}
	for id := range graph {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func jobKey(j Job) string {
	data, _ := json.Marshal(struct {
		Spec     any
		Workflow string
		Depends  []string
		From     map[string]string
		Retry    RetryPolicy
	}{j.Spec, j.Workflow, j.Depends, j.From, j.Retry})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
func (s *Server) serveWorkflow(w http.ResponseWriter, r *http.Request) {
	var flow Workflow
	if err := DecodeJSON(http.MaxBytesReader(w, r.Body, 8<<20), &flow); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	jobs, err := s.SubmitWorkflow(r.Context(), flow)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	WriteJSON(w, http.StatusOK, jobs)
}

func (s *Server) SubmitWorkflow(ctx context.Context, w Workflow) ([]Job, error) {
	steps, err := w.scope()
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(steps))
	requests := make([]Request, len(steps))
	for i, step := range steps {
		if err := s.check(step.Request); err != nil {
			return nil, err
		}
		requests[i] = step.Request
	}
	st := s.stage(ctx, requests...)
	for _, step := range steps {
		j, err := s.prepare(step.Request, step.From, st)
		if err != nil {
			return nil, err
		}
		j.Workflow = w.ID
		j.Depends = step.Depends
		j.Key = jobKey(j)
		jobs = append(jobs, j)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return nil, s.fault
	}
	existing := 0
	for _, old := range s.jobs {
		if old.Workflow == w.ID {
			existing++
		}
	}
	for i, j := range jobs {
		if old, ok := s.jobs[j.ID]; ok {
			if old.Workflow != w.ID || old.Key != j.Key {
				return nil, errors.New("workflow ID conflicts with existing specification")
			}
			jobs[i] = old
		} else if existing > 0 {
			return nil, errors.New("workflow ID has a different task set")
		}
	}
	if existing > 0 {
		if existing != len(jobs) {
			return nil, errors.New("workflow ID has a different task set")
		}
		return jobs, nil
	}
	if err := s.commitLocked(jobs...); err != nil {
		return nil, err
	}
	return jobs, nil
}
func (s *Server) dependencies(j *Job) (bool, error) {
	s.mu.Lock()
	deps := map[string]Job{}
	for _, id := range j.Depends {
		dep, ok := s.jobs[id]
		if !ok {
			s.mu.Unlock()
			return false, errors.New("missing persisted dependency")
		}
		deps[id] = dep
	}
	s.mu.Unlock()
	for _, dep := range deps {
		if dep.State.Terminal() && dep.State != Succeeded {
			j.State = Skipped
			j.Error = "dependency did not succeed"
			return false, s.save(*j)
		}
	}
	for _, dep := range deps {
		if dep.State != Succeeded {
			return false, nil
		}
	}
	return true, nil
}

// importOutputs runs only once a worker fits, so a queued job never re-imports outputs per tick.
func (s *Server) importOutputs(j *Job) error {
	if len(j.Attempts) > 0 {
		return nil
	}
	s.mu.Lock()
	deps := map[string]Job{}
	for _, id := range j.From {
		deps[id] = s.jobs[id]
	}
	s.mu.Unlock()
	j.Spec.Inputs = maps.Clone(j.Spec.Inputs)
	if j.Spec.Inputs == nil {
		j.Spec.Inputs = map[string]string{}
	}
	for name, id := range j.From {
		dep := deps[id]
		at := dep.current()
		if at.Result == nil {
			return errors.New("dependency has no output identity")
		}
		hash := at.Result.OutputsSHA256
		// Collection verified the archive against this digest, and nothing rewrites a result directory.
		if err := content.Link(s.images.Blobs, filepath.Join(at.ResultDir, OutputsArchive), hash); err != nil {
			return err
		}
		j.Spec.Inputs[name] = hash
	}
	return nil
}
func (s *Server) Cancel(id string, workflow bool, reason string) error {
	return s.requestStop(id, workflow, reason, func(j *Job) { j.CancelRequested = true })
}

func (s *Server) Stop(id, reason string) error {
	return s.requestStop(id, false, reason, func(j *Job) { j.StopRequested = true })
}

func (s *Server) requestStop(id string, workflow bool, reason string, mark func(*Job)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var targets []Job
	if !workflow {
		if j, ok := s.jobs[id]; ok {
			targets = append(targets, j)
		}
	} else {
		for _, j := range s.jobs {
			if j.Workflow == id {
				targets = append(targets, j)
			}
		}
	}
	var changed []Job
	for _, j := range targets {
		if j.State.Terminal() {
			continue
		}
		mark(&j)
		j.StopReason = cmp.Or(j.StopReason, reason)
		if j.State == Queued && !s.busy[j.ID] {
			j.State = Cancelled
		}
		changed = append(changed, j)
	}
	return s.commitLocked(changed...)
}
func (s *Server) finish(j Job, class Failure) error {
	s.refresh(&j)
	j.current().Failure = class
	count := 0
	for _, at := range j.Attempts {
		if at.Failure == class {
			count++
		}
	}
	switch {
	case j.CancelRequested || class == CancelFailure:
		j.State = Cancelled
	case class == "":
		j.State = Succeeded
	case j.StopRequested || count > j.Retry.limit(class):
		j.State = Failed
	default:
		j.State = Queued
	}
	return s.save(j)
}
