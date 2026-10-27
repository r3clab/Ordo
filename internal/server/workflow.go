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
	"path/filepath"
	"slices"
	"strings"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/image"
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
	ID    string       `json:"id"`
	Image image.Source `json:"image,omitzero"`
	Tasks []Step       `json:"tasks"`
}
type Step struct {
	Request
	Depends    []string            `json:"depends_on,omitempty"`
	From       map[string]string   `json:"from,omitempty"`
	Matrix     map[string][]string `json:"matrix,omitempty"`
	Parameters map[string]string   `json:"-"`
}

func (w Workflow) nix() bool {
	return w.Image.IsNix() || slices.ContainsFunc(w.Tasks, func(step Step) bool { return step.Image.IsNix() })
}

func expand(w Workflow) ([]Step, error) {
	if !spec.ValidName(w.ID) || len(w.ID) > MaxWorkflowID || len(w.Tasks) == 0 || len(w.Tasks) > MaxWorkflowTasks {
		return nil, fmt.Errorf("workflow needs an ID (1..%d characters) and 1..%d tasks", MaxWorkflowID, MaxWorkflowTasks)
	}
	groups := map[string][]string{}
	ids := map[string]bool{}
	var expanded []Step
	for _, step := range w.Tasks {
		if !spec.ValidName(step.ID) || len(step.ID) > MaxStepID || groups[step.ID] != nil {
			return nil, fmt.Errorf("task IDs must be unique and 1..%d characters", MaxStepID)
		}
		instances, err := step.instances()
		if err != nil {
			return nil, err
		}
		for _, t := range instances {
			if ids[t.ID] {
				return nil, errors.New("expanded task IDs collide")
			}
			ids[t.ID] = true
			groups[step.ID] = append(groups[step.ID], t.ID)
			if t.Image.IsZero() {
				t.Image = w.Image
			}
			expanded = append(expanded, t)
		}
		if len(expanded) > MaxWorkflowTasks {
			return nil, fmt.Errorf("expanded workflow exceeds %d tasks", MaxWorkflowTasks)
		}
	}
	graph, err := link(w.ID, expanded, func(id string) []string {
		if group, ok := groups[id]; ok {
			return group
		}
		if ids[id] {
			return []string{id}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return expanded, acyclic(graph)
}

func (step Step) instances() ([]Step, error) {
	combos := []map[string]string{{}}
	for _, key := range slices.Sorted(maps.Keys(step.Matrix)) {
		values := step.Matrix[key]
		if !spec.ValidName(key) || len(values) == 0 || len(values) > MaxWorkflowTasks || len(combos) > MaxWorkflowTasks/len(values) {
			return nil, errors.New("invalid or oversized parameter matrix")
		}
		var next []map[string]string
		for _, params := range combos {
			for _, value := range values {
				p := maps.Clone(params)
				p[key] = value
				next = append(next, p)
			}
		}
		combos = next
	}
	instances := make([]Step, len(combos))
	for i, params := range combos {
		t := step
		t.Parameters = params
		t.From = maps.Clone(step.From)
		if len(step.Matrix) > 0 {
			t.ID = fmt.Sprintf("%s-%d", step.ID, i)
		}
		var err error
		if t.Args, err = substitute(step.Args, params); err != nil {
			return nil, err
		}
		instances[i] = t
	}
	return instances, nil
}

func substitute(args []string, params map[string]string) ([]string, error) {
	var pairs []string
	for _, key := range slices.Sorted(maps.Keys(params)) {
		pairs = append(pairs, "{{"+key+"}}", params[key])
	}
	replacer := strings.NewReplacer(pairs...)
	args = slices.Clone(args)
	for i, arg := range args {
		args[i] = replacer.Replace(arg)
		if strings.Contains(args[i], "{{") {
			return nil, errors.New("unknown parameter placeholder")
		}
	}
	return args, nil
}

func link(workflow string, tasks []Step, resolve func(id string) []string) (map[string][]string, error) {
	scoped := func(id string) string { return workflow + "--" + id }
	graph := map[string][]string{}
	for i, step := range tasks {
		deps := []string{}
		for _, dep := range step.Depends {
			targets := resolve(dep)
			if len(targets) == 0 {
				return nil, fmt.Errorf("unknown dependency %s", dep)
			}
			deps = append(deps, targets...)
		}
		for name, from := range step.From {
			if !spec.ValidName(name) {
				return nil, errors.New("invalid dependency input name")
			}
			if _, ok := step.Inputs[name]; ok {
				return nil, errors.New("input has both local and dependency sources")
			}
			targets := resolve(from)
			if len(targets) != 1 {
				return nil, errors.New("dependency input must select exactly one expanded task")
			}
			deps = append(deps, targets[0])
			step.From[name] = scoped(targets[0])
		}
		slices.Sort(deps)
		deps = slices.Compact(deps)
		graph[step.ID] = deps
		step.Depends = nil
		for _, dep := range deps {
			step.Depends = append(step.Depends, scoped(dep))
		}
		step.ID = scoped(step.ID)
		tasks[i] = step
	}
	return graph, nil
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
		Spec       any
		Workflow   string
		Depends    []string
		From       map[string]string
		Parameters map[string]string
		Retry      RetryPolicy
	}{j.Spec, j.Workflow, j.Depends, j.From, j.Parameters, j.Retry})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}
func (s *Server) SubmitWorkflow(ctx context.Context, w Workflow) ([]Job, error) {
	steps, err := expand(w)
	if err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(steps))
	secrets := map[string]map[string]string{}
	requests := make([]Request, len(steps))
	for i, step := range steps {
		requests[i] = step.Request
	}
	st := s.stage(ctx, requests...)
	for _, step := range steps {
		j, err := s.prepare(ctx, step.Request, step.From, st)
		if err != nil {
			return nil, err
		}
		if len(step.Secrets) > 0 {
			secrets[j.ID] = step.Secrets
		}
		j.Workflow = w.ID
		j.Depends = step.Depends
		j.Parameters = step.Parameters
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
	if err := s.commitLocked(secrets, jobs...); err != nil {
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
	if s.fault != nil {
		return s.fault
	}
	var changed []Job
	for key, j := range s.jobs {
		if (!workflow && key != id) || (workflow && j.Workflow != id) {
			continue
		}
		if j.State.Terminal() {
			continue
		}
		mark(&j)
		j.StopReason = cmp.Or(j.StopReason, reason)
		if j.State == Queued && !s.busy[key] {
			j.State = Cancelled
		}
		changed = append(changed, j)
	}
	return s.commitLocked(nil, changed...)
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
