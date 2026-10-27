package server

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/worker"
)

func TestWorkflowScoping(t *testing.T) {
	w := Workflow{ID: "flow", Tasks: []Step{{Request: Request{ID: "map"}}, {Request: Request{ID: "reduce"}, Depends: []string{"map"}, From: map[string]string{"first": "map"}}}}
	for range 2 {
		steps, err := w.scope()
		if err != nil {
			t.Fatal(err)
		}
		if steps[0].ID != "flow--map" || steps[1].ID != "flow--reduce" || !slices.Equal(steps[1].Depends, []string{"flow--map"}) || steps[1].From["first"] != "flow--map" {
			t.Fatalf("bad scoping: %+v", steps)
		}
	}
	if w.Tasks[1].ID != "reduce" || w.Tasks[1].From["first"] != "map" {
		t.Fatal("scoping changed the submitted workflow")
	}
	for name, tasks := range map[string][]Step{
		"a cycle":                   {{Request: Request{ID: "a"}, Depends: []string{"b"}}, {Request: Request{ID: "b"}, Depends: []string{"a"}}},
		"a missing dependency":      {{Request: Request{ID: "a"}, Depends: []string{"missing"}}},
		"a missing input source":    {{Request: Request{ID: "a"}, From: map[string]string{"x": "missing"}}},
		"a repeated task ID":        {{Request: Request{ID: "a"}}, {Request: Request{ID: "a"}}},
		"an input with two sources": {{Request: Request{ID: "a"}}, {Request: Request{ID: "b", Inputs: map[string]string{"x": "/data"}}, From: map[string]string{"x": "a"}}},
	} {
		if _, err := (Workflow{ID: "bad", Tasks: tasks}).scope(); err == nil {
			t.Fatal("accepted", name)
		}
	}
}
func TestWorkflowAtomicSubmissionAndRetries(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "state"), []worker.Config{testWorker("worker")}, nil)
	defer func() { _ = s.Close() }()
	req := request("")
	a := Step{Request: req}
	a.ID = "a"
	a.Retry.Task = 1
	b := Step{Request: req}
	b.ID = "b"
	b.Depends = []string{"a"}
	flow := Workflow{ID: "flow", Tasks: []Step{a, b}}
	for _, change := range []func(*Step){func(step *Step) { step.Resources.Cores = 0 }, func(step *Step) { step.Image = image.Source{} }} {
		bad := flow
		bad.Tasks = append([]Step(nil), flow.Tasks...)
		change(&bad.Tasks[1])
		if _, err := s.SubmitWorkflow(context.Background(), bad); err == nil || len(s.List()) != 0 {
			t.Fatal("published partial workflow")
		}
	}
	jobs, err := s.SubmitWorkflow(context.Background(), flow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitWorkflow(context.Background(), flow); err != nil {
		t.Fatal("idempotent workflow rejected", err)
	}
	j := jobs[0]
	j.Attempts = []Attempt{{ID: "one"}}
	if err := s.finish(j, "task"); err != nil {
		t.Fatal(err)
	}
	j = s.List()[0]
	if j.State != "queued" {
		t.Fatal("retry omitted")
	}
	j.Attempts = append(j.Attempts, Attempt{ID: "two"})
	if err := s.finish(j, "task"); err != nil {
		t.Fatal(err)
	}
	if s.List()[0].State != "failed" {
		t.Fatal("unbounded retry")
	}
	dependent := jobs[1]
	if ready, err := s.dependencies(&dependent); err != nil || ready {
		t.Fatal("failed dependency advanced", err)
	}
	if dependent.State != "skipped" {
		t.Fatal("dependent was not skipped")
	}
}
