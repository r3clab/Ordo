package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/worker"
)

func twoSteps(out string) Request {
	r := jobRequest(out)
	first, second := r.Step, r.Step
	first.Name = "klee"
	second.Resources.Cores = 4
	r.Step, r.Steps = Step{}, []Step{first, second}
	return r
}

func TestStepsRunInOrderFromEachOthersOutputs(t *testing.T) {
	f := newFixture(t)
	out := f.out("grep/dfs/01")
	id := f.submit(t, twoSteps(out))
	f.tick(t)
	first := last(f.job(t, id))
	if len(f.h.launches) != 1 || f.h.launches[0].Spec.Resources.Cores != 1 || len(f.h.seeds) != 0 {
		t.Fatalf("first step launch %+v, seeds %v", f.h.launches, f.h.seeds)
	}
	f.h.exit(first.ID, 0)
	f.tick(t)
	if j := f.job(t, id); j.State != Queued || j.Step != 1 {
		t.Fatalf("a finished step must queue the next: %+v", j)
	}
	f.tick(t)
	j := f.job(t, id)
	second := j.current()
	if j.State != Running || second.Step != 1 || len(f.h.launches) != 2 || f.h.launches[1].Spec.Resources.Cores != 4 || !slices.Equal(f.h.seeds, []seeded{{second.ID, first.ID, true}}) {
		t.Fatalf("second step must start from the first's outputs: %+v, seeds %v", j, f.h.seeds)
	}
	f.h.exit(second.ID, 0)
	f.tick(t)
	f.tick(t)
	if j = f.job(t, id); j.State != Succeeded {
		t.Fatalf("job not delivered: %+v", j)
	}
	if readOut(t, out, "score") != second.ID || readOut(t, out, ".ordo/klee.stdout") != "out" || readOut(t, out, ".ordo/2.stderr") != "err" {
		t.Fatal("out lacks the last step's outputs or a step's logs")
	}
	entries, err := os.ReadDir(filepath.Dir(out))
	if err != nil || len(entries) != 1 {
		t.Fatalf("delivery left staging beside out: %v %v", entries, err)
	}
}

func TestNextStepPrefersTheWorkerThatKeptItsInputs(t *testing.T) {
	workers := []worker.Config{testWorker("a"), testWorker("b")}
	s := open(t, filepath.Join(t.TempDir(), "state"), workers, nil)
	defer func() { _ = s.Close() }()
	a, b := newFakeHost(s.id), newFakeHost(s.id)
	id := submit(t, s, twoSteps(filepath.Join(t.TempDir(), "job")))
	mustTick(t, s, workers[0], a)
	first := last(mustJob(t, s, id))
	a.exit(first.ID, 0)
	mustTick(t, s, workers[0], a)
	if !a.runs[first.ID] || len(a.containers) != 0 {
		t.Fatalf("the first step's outputs must stay on its worker without its container: %v", a.runs)
	}
	mustTick(t, s, workers[1], b)
	if len(b.launches) != 0 {
		t.Fatal("another worker took the step before the kept outputs' worker had a chance")
	}
	a.add("foreign", "other", podman.Running)
	mustTick(t, s, workers[0], a)
	if len(a.launches) != 1 || !a.runs[first.ID] {
		t.Fatal("a sweep removed outputs a queued step is to start from")
	}
	delete(a.containers, "foreign")
	j := mustJob(t, s, id)
	j.ReadyAt = j.ReadyAt.Add(-stepAffinity)
	seed(t, s, j)
	mustTick(t, s, workers[1], b)
	second := last(mustJob(t, s, id))
	if second.Worker != "b" || !slices.Equal(b.seeds, []seeded{{second.ID, first.ID, false}}) {
		t.Fatalf("a worker without the outputs must start from the server's copy: %+v %v", second, b.seeds)
	}
	mustTick(t, s, workers[0], a)
	if a.runs[first.ID] {
		t.Fatal("kept outputs outlived the step that needed them")
	}
}

func TestRetriesCountPerStepAndTaskFailuresAreDelivered(t *testing.T) {
	f := newFixture(t)
	req := twoSteps(f.out("job"))
	req.Retry.Task = 1
	id := f.submit(t, req)
	exit := func(code int) {
		t.Helper()
		f.tick(t)
		f.h.exit(last(f.job(t, id)).ID, code)
		f.tick(t)
		if f.job(t, id).State == Queued {
			ready(t, f.s, id)
		}
	}
	exit(1)
	exit(0)
	exit(1)
	if j := f.job(t, id); j.State != Queued || j.Step != 1 {
		t.Fatalf("the first step's failure used up the second's retry: %+v", j)
	}
	exit(1)
	f.tick(t)
	j := f.job(t, id)
	if j.State != Failed || len(j.Attempts) != 4 {
		t.Fatalf("an exhausted task failure must fail the job: %+v", j)
	}
	var published Job
	if err := json.Unmarshal([]byte(readOut(t, f.out("job"), ".ordo/job.json")), &published); err != nil || published.State != Failed {
		t.Fatalf("a failed job must still deliver its last outputs: %+v %v", published, err)
	}
}

func TestStopEndsOnlyTheRunningStep(t *testing.T) {
	f := newFixture(t)
	id := f.submit(t, twoSteps(f.out("job")))
	f.tick(t)
	if err := f.s.Stop(id, "enough"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	first := last(f.job(t, id))
	f.h.exit(first.ID, 0)
	f.tick(t)
	j := f.job(t, id)
	if j.State != Queued || j.Step != 1 || j.StopRequested || j.Attempts[0].StopReason != "enough" {
		t.Fatalf("a step that exits zero after a stop must hand over to the next: %+v", j)
	}
	f.tick(t)
	if len(f.h.stops) != 1 || f.job(t, id).State != Running {
		t.Fatalf("the stop followed the job to its next step: %q", f.h.stops)
	}
}

func TestAdmissionIsKeyedByOut(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	id := f.submit(t, jobRequest(f.out("a")))
	if again := f.submit(t, jobRequest(f.out("a"))); again != id {
		t.Fatal("resubmission made a second job for one out")
	}
	changed := jobRequest(f.out("a"))
	changed.Args = []string{"other"}
	for name, r := range map[string]Request{"a different request": changed, "an out inside another's": jobRequest(f.out("a/b")), "an out around another's": jobRequest(f.dir)} {
		if _, err := f.s.Submit(ctx, r); err == nil {
			t.Fatal("accepted", name)
		}
	}
	f.tick(t)
	f.h.exit(last(f.job(t, id)).ID, 0)
	f.tick(t)
	f.tick(t)
	if _, err := f.s.Submit(ctx, jobRequest(f.out("a"))); err == nil || !strings.Contains(err.Error(), "already holds results") {
		t.Fatalf("a delivered out must count as done: %v", err)
	}
	if err := os.RemoveAll(f.out("a")); err != nil {
		t.Fatal(err)
	}
	if redo := f.submit(t, jobRequest(f.out("a"))); redo == id {
		t.Fatal("removing out must let the job run again")
	}
}

func TestDeliveryWaitsForAnOccupiedOut(t *testing.T) {
	f := newFixture(t)
	out := f.out("job")
	id := f.submit(t, jobRequest(out))
	f.tick(t)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "stray"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.h.exit(last(f.job(t, id)).ID, 0)
	f.tick(t)
	f.tick(t)
	if j := f.job(t, id); j.State != Delivering || !strings.Contains(j.Error, "deliver results") {
		t.Fatalf("delivery into an occupied out must wait: %+v", j)
	}
	if err := os.Remove(filepath.Join(out, "stray")); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j := f.job(t, id); j.State != Succeeded || j.Error != "" {
		t.Fatalf("delivery did not resume: %+v", j)
	}
}

func TestDeliveryInterruptedAfterRenameCompletes(t *testing.T) {
	f := newFixture(t)
	out := f.out("job")
	id := f.submit(t, jobRequest(out))
	f.tick(t)
	f.h.exit(last(f.job(t, id)).ID, 0)
	f.tick(t)
	j := f.job(t, id)
	if err := os.MkdirAll(filepath.Join(out, MetaDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(out, MetaDir, JobFile), j); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j = f.job(t, id); j.State != Succeeded {
		t.Fatalf("a published job.json must settle delivery: %+v", j)
	}
}

func TestInputsInAnotherJobsOutWaitForIt(t *testing.T) {
	f := newFixture(t)
	up := f.submit(t, jobRequest(f.out("up")))
	down := jobRequest(f.out("down"))
	down.Inputs = map[string]string{"up": f.out("up")}
	downID := f.submit(t, down)
	if j := f.job(t, downID); !slices.Equal(j.Waiting, []string{"up"}) || !slices.Equal(j.After, []string{up}) {
		t.Fatalf("an input in an unfinished job's out must wait for it: %+v", j)
	}
	f.tick(t)
	if len(f.h.launches) != 1 {
		t.Fatal("a job ran before the job it takes inputs from")
	}
	f.h.exit(last(f.job(t, up)).ID, 0)
	for range 3 {
		f.tick(t)
	}
	if again := f.submit(t, down); again != downID {
		t.Fatal("a resubmission did not match once the input was snapshotted")
	}
	f.tick(t)
	if len(f.h.launches) != 2 || !content.Valid(f.h.launches[1].Spec.Inputs["up"]) || len(f.job(t, downID).Waiting) != 0 {
		t.Fatalf("the waiting input was not snapshotted from the delivered out: %+v", f.h.launches)
	}

	cancelled := f.submit(t, jobRequest(f.out("cancelled")))
	skipped := jobRequest(f.out("skipped"))
	skipped.Inputs = map[string]string{"x": f.out("cancelled")}
	skippedID := f.submit(t, skipped)
	if err := f.s.Cancel(cancelled, ""); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j := f.job(t, skippedID); j.State != Skipped {
		t.Fatalf("a job whose input's job was cancelled must be skipped: %+v", j)
	}

	first, second := jobRequest(f.out("first")), jobRequest(f.out("second"))
	second.Inputs = map[string]string{"first": filepath.Join(f.out("first"), "sub")}
	results := slices.Collect(f.s.SubmitBatch(context.Background(), []BatchEntry{{Line: 1, Request: second}, {Line: 2, Request: first}, {Line: 3, Request: func() Request { r := second; r.Out = f.out("third"); return r }()}}))
	if results[0].Error == "" || results[1].Error != "" || results[2].Error != "" {
		t.Fatalf("an input may wait only for an earlier line: %+v", results)
	}
}

func TestRequestValidation(t *testing.T) {
	both := twoSteps("/results/a")
	both.Image = "/src#image"
	named := twoSteps("/results/a")
	named.Steps[1].Name = "klee"
	input := jobRequest("/results/a")
	input.Inputs = map[string]string{"data": "/results/a/x"}
	relative := jobRequest("/results/a")
	relative.Inputs = map[string]string{"data": "data"}
	many := jobRequest("/results/a")
	many.Steps = slices.Repeat([]Step{{Image: "/src#image", Task: spec.Task{Resources: spec.Resources{Cores: 1, MemoryMiB: 1}}}}, MaxSteps+1)
	many.Step = Step{}
	for name, r := range map[string]Request{
		"a relative out":                 jobRequest("results/a"),
		"an unclean out":                 jobRequest("/results/../a"),
		"/ as out":                       jobRequest("/"),
		"steps beside top-level fields":  both,
		"repeated step names":            named,
		"an input inside out":            input,
		"a relative input":               relative,
		"more steps than the limit":      many,
		"a step without resources":       {Out: "/results/a", Steps: []Step{{Image: "/src#image"}}},
		"a job without an image or step": {Out: "/results/a"},
	} {
		if err := r.Validate(); err == nil {
			t.Error("accepted", name)
		}
	}
	if err := twoSteps("/results/a").Validate(); err != nil {
		t.Fatal(err)
	}
	if err := jobRequest("/results/a").Validate(); err != nil {
		t.Fatal(err)
	}
}

func last(j Job) Attempt { return j.Attempts[len(j.Attempts)-1] }
