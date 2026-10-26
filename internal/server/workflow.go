package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/worker"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

type RetryPolicy struct {
	Task           int  `json:"task,omitempty"`
	Infrastructure *int `json:"infrastructure,omitempty"`
}

func (r RetryPolicy) validate() error {
	if r.Task < 0 || r.Task > 10 || (r.Infrastructure != nil && (*r.Infrastructure < 0 || *r.Infrastructure > 10)) {
		return errors.New("retry counts must be 0..10")
	}
	return nil
}
func (r RetryPolicy) infra() int {
	if r.Infrastructure == nil {
		return 1
	}
	return *r.Infrastructure
}

type Workflow struct {
	ID      string `json:"id"`
	Image   string `json:"image,omitempty"`
	Archive string `json:"archive,omitempty"`
	Tasks   []Step `json:"tasks"`
}
type Step struct {
	Request
	Depends    []string            `json:"depends_on,omitempty"`
	From       map[string]string   `json:"from,omitempty"`
	Matrix     map[string][]string `json:"matrix,omitempty"`
	Parameters map[string]string   `json:"-"`
}

func expand(w Workflow) ([]Step, error) {
	if !worker.ValidName(w.ID) || len(w.ID) > 24 || len(w.Tasks) == 0 || len(w.Tasks) > 1000 {
		return nil, errors.New("workflow needs an ID (1..24 characters) and 1..1000 tasks")
	}
	groups := map[string][]string{}
	ids := map[string]bool{}
	var expanded []Step
	for _, step := range w.Tasks {
		if !worker.ValidName(step.ID) || len(step.ID) > 32 || groups[step.ID] != nil {
			return nil, errors.New("task IDs must be unique and 1..32 characters")
		}
		combos := []map[string]string{{}}
		for _, key := range slices.Sorted(maps.Keys(step.Matrix)) {
			values := step.Matrix[key]
			if !worker.ValidName(key) || len(values) == 0 || len(values) > 1000 || len(combos) > 1000/len(values) {
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
		for i, params := range combos {
			task := step
			task.Parameters = params
			task.Args = slices.Clone(step.Args)
			task.Inputs = maps.Clone(step.Inputs)
			task.From = maps.Clone(step.From)
			task.Depends = slices.Clone(step.Depends)
			if len(step.Matrix) > 0 {
				task.ID = fmt.Sprintf("%s-%d", step.ID, i)
			}
			if ids[task.ID] {
				return nil, errors.New("expanded task IDs collide")
			}
			ids[task.ID] = true
			groups[step.ID] = append(groups[step.ID], task.ID)
			replacements := []string{}
			for _, key := range slices.Sorted(maps.Keys(params)) {
				replacements = append(replacements, "{{"+key+"}}", params[key])
			}
			replacer := strings.NewReplacer(replacements...)
			for i, arg := range task.Args {
				arg = replacer.Replace(arg)
				if strings.Contains(arg, "{{") {
					return nil, errors.New("unknown parameter placeholder")
				}
				task.Args[i] = arg
			}
			if task.Image == "" {
				task.Image = w.Image
				if task.Archive == "" {
					task.Archive = w.Archive
				}
			}
			if err := task.Retry.validate(); err != nil {
				return nil, err
			}
			expanded = append(expanded, task)
			if len(expanded) > 1000 {
				return nil, errors.New("expanded workflow exceeds 1000 tasks")
			}
		}
	}
	resolve := func(id string) []string {
		if group, ok := groups[id]; ok {
			return group
		}
		if ids[id] {
			return []string{id}
		}
		return nil
	}
	graph := map[string][]string{}
	for i, step := range expanded {
		deps := []string{}
		for _, dep := range step.Depends {
			targets := resolve(dep)
			if len(targets) == 0 {
				return nil, fmt.Errorf("unknown dependency %s", dep)
			}
			deps = append(deps, targets...)
		}
		for name, from := range step.From {
			if !worker.ValidName(name) {
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
			step.From[name] = w.ID + "--" + targets[0]
		}
		slices.Sort(deps)
		deps = slices.Compact(deps)
		graph[step.ID] = deps
		step.Depends = nil
		for _, dep := range deps {
			step.Depends = append(step.Depends, w.ID+"--"+dep)
		}
		step.ID = w.ID + "--" + step.ID
		expanded[i] = step
	}
	visited := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if visited[id] == 1 {
			return errors.New("workflow dependency cycle")
		}
		if visited[id] == 2 {
			return nil
		}
		visited[id] = 1
		for _, dep := range graph[id] {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visited[id] = 2
		return nil
	}
	for id := range graph {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return expanded, nil
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
	st := newStaging()
	for _, step := range steps {
		j, err := s.prepare(ctx, step.Request, st)
		if err != nil {
			return nil, err
		}
		j.Workflow = w.ID
		j.Depends = step.Depends
		j.From = step.From
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
		if dep.Terminal() && dep.State != "succeeded" {
			j.State = "skipped"
			j.Error = "dependency did not succeed"
			return false, s.save(*j)
		}
	}
	for _, dep := range deps {
		if dep.State != "succeeded" {
			return false, nil
		}
	}
	return true, nil
}

// importOutputs runs only once a worker fits, so a queued job never re-copies outputs per tick.
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
		at := dep.Attempts[len(dep.Attempts)-1]
		if at.Remote == nil || at.Remote.Result == nil {
			return errors.New("dependency has no output identity")
		}
		hash := at.Remote.Result.OutputsSHA256
		if _, err := content.PutFile(filepath.Join(s.Root, "blobs"), filepath.Join(at.ResultDir, "outputs.tar"), hash); err != nil {
			return err
		}
		j.Spec.Inputs[name] = hash
	}
	return nil
}
func (s *Server) Cancel(id string, workflow bool) error {
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
		if j.Terminal() {
			continue
		}
		j.CancelRequested = true
		if j.State == "queued" && !s.busy[key] {
			j.State = "cancelled"
		}
		changed = append(changed, j)
	}
	if len(changed) == 0 {
		return nil
	}
	return s.commitLocked(changed...)
}
func (s *Server) finish(j Job, class string) error {
	at := &j.Attempts[len(j.Attempts)-1]
	at.Failure = class
	if j.CancelRequested || class == "cancel" {
		j.State = "cancelled"
		return s.save(j)
	}
	if class == "" {
		j.State = "succeeded"
		return s.save(j)
	}
	count := 0
	for _, attempt := range j.Attempts {
		if attempt.Failure == class {
			count++
		}
	}
	limit := j.Retry.Task
	if class == "infrastructure" {
		limit = j.Retry.infra()
	}
	j.State = "failed"
	if count <= limit {
		j.State = "queued"
	}
	return s.save(j)
}
