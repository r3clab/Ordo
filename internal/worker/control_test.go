package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCancellationTombstonePreventsLateLaunch(t *testing.T) {
	a, cfg, h := testAgent(t)
	if err := a.Cancel("late", ""); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := open(context.Background(), cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	if _, err := a.Submit("late", testSpec(), nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("late launch admitted: %v", err)
	}
}
func TestCancelAndTimeoutBeforeLaunchReleaseOnlyUnstartedWork(t *testing.T) {
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	run, err := a.Submit("cancel", testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Cancel(run.ID, ""); err != nil {
		t.Fatal(err)
	}
	if a.attempts[run.ID].State != "cancelled" {
		t.Fatal("unstarted task not cancelled")
	}
	spec := testSpec()
	spec.TimeoutSeconds = 1
	run, err = a.Submit("timeout", spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	run.CreatedAt = time.Now().Add(-2 * time.Second)
	if err := a.save(run); err != nil {
		t.Fatal(err)
	}
	a.enforceStops()
	if got := a.attempts[run.ID]; !got.Terminal() || got.StopRequested != "timeout" {
		t.Fatalf("deadline ignored: %+v", got)
	}
}
