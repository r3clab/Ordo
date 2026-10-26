package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/worker"
)

func TestWorkflowExpansionAndValidation(t *testing.T) {
	w := Workflow{ID: "sweep", Tasks: []Step{{Request: Request{ID: "map", Args: []string{"{{seed}}"}}, Matrix: map[string][]string{"seed": {"1", "2"}}}, {Request: Request{ID: "reduce"}, Depends: []string{"map"}, From: map[string]string{"first": "map-0"}}}}
	steps, err := expand(w)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 3 || steps[0].Args[0] != "1" || steps[1].Args[0] != "2" || len(steps[2].Depends) != 2 || steps[2].From["first"] != "sweep--map-0" {
		t.Fatalf("bad expansion: %+v", steps)
	}
	w.Tasks[1].From["first"] = "map"
	if _, err := expand(w); err == nil {
		t.Fatal("accepted ambiguous sweep input")
	}
	w = Workflow{ID: "cycle", Tasks: []Step{{Request: Request{ID: "a"}, Depends: []string{"b"}}, {Request: Request{ID: "b"}, Depends: []string{"a"}}}}
	if _, err := expand(w); err == nil {
		t.Fatal("accepted cycle")
	}
	w.Tasks[1].Depends = []string{"missing"}
	if _, err := expand(w); err == nil {
		t.Fatal("accepted missing dependency")
	}
}
func TestWorkflowAtomicSubmissionAndRetries(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "image.tar")
	if err := os.WriteFile(archive, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(root, "state"), []worker.Remote{{Host: "worker"}}, nil, image.Tools{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	req := Request{Image: image.Source{Ref: "sha256:" + strings.Repeat("a", 64)}, Archive: archive, Resources: worker.Resources{Cores: 1, MemoryMiB: 128}}
	a := Step{Request: req}
	a.ID = "a"
	a.Retry.Task = 1
	b := Step{Request: req}
	b.ID = "b"
	b.Depends = []string{"a"}
	flow := Workflow{ID: "flow", Tasks: []Step{a, b}}
	bad := flow
	bad.Tasks = append([]Step(nil), flow.Tasks...)
	bad.Tasks[1].Resources.Cores = 0
	if _, err := s.SubmitWorkflow(context.Background(), Local, bad); err == nil || len(s.List()) != 0 {
		t.Fatal("published partial workflow")
	}
	jobs, err := s.SubmitWorkflow(context.Background(), Local, flow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SubmitWorkflow(context.Background(), Local, flow); err != nil {
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
