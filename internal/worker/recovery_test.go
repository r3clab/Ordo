package worker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/task"
)

func savedRun(t *testing.T, a *Agent, run Attempt, r task.Record) {
	t.Helper()
	dir := a.runDir(run.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{"run.json": data, "stdout.log": {}, "stderr.log": {}} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconcileCompletedExecutionWithoutPodman(t *testing.T) {
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	run, err := a.Submit("completed", testSpec())
	if err != nil {
		t.Fatal(err)
	}
	r := task.Record{Request: task.Request{Image: run.Spec.Image, CPUs: CPUString(run.Allocation.CPUs), MemoryMiB: run.Allocation.MemoryMiB, OutputDir: a.runDir(run.ID), ReservationID: a.info.ID + "/" + run.ID}, ContainerName: "ordo-completed", ContainerID: strings.Repeat("b", 64), ImageID: run.Spec.Image, State: "finished", ContainerRemoved: true, ExitCode: new(7)}
	savedRun(t, a, run, r)
	a.wg.Add(1)
	a.execute(run, false)
	if got := a.attempts[run.ID]; got.State != "finished" || got.Result == nil || *got.Result.ExitCode != 7 {
		t.Fatalf("lost completed execution: %+v", got)
	}
	spec := testSpec()
	spec.Resources.Cores = 2
	spec.Resources.MemoryMiB = 512
	if _, err := a.Submit("after", spec); err != nil {
		t.Fatalf("completed reservation not released: %v", err)
	}
}

func TestAmbiguousLaunchAndMismatchedRecordKeepReservation(t *testing.T) {
	for _, mismatched := range []bool{false, true} {
		a, _, _ := testAgent(t)
		spec := testSpec()
		spec.Resources.Cores = 2
		spec.Resources.MemoryMiB = 512
		run, err := a.Submit("unknown", spec)
		if err != nil {
			t.Fatal(err)
		}
		if mismatched {
			savedRun(t, a, run, task.Record{ContainerName: "ordo-other", State: "preparing"})
		}
		a.wg.Add(1)
		a.execute(run, false)
		if got := a.attempts[run.ID]; got.State != "blocked" {
			t.Fatalf("ambiguous task released: %+v", got)
		}
		if _, err := a.Submit("other", testSpec()); !errors.Is(err, ErrCapacity) {
			t.Fatalf("lost reservation: %v", err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPersistenceFailureStopsAdmissions(t *testing.T) {
	a, _, _ := testAgent(t)
	if _, err := a.Submit("one", testSpec()); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Submit("two", testSpec()); err == nil {
		t.Fatal("accepted request without durability")
	}
	if _, err := a.Submit("three", testSpec()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed to latch admission closed: %v", err)
	}
	a.cancel()
}
