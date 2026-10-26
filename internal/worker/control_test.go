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
	if got := a.attempts[run.ID]; !got.State.Terminal() || got.StopRequested != "timeout" {
		t.Fatalf("deadline ignored: %+v", got)
	}
}

func TestStopRetriesBackOff(t *testing.T) {
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	run, err := a.Submit("run", testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	a.busy[run.ID] = true
	if err := a.Stop(run.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		a.enforceStops()
		a.wg.Wait()
		a.enforceStops()
		retry := a.stops[run.ID]
		if retry.active || retry.delay != want || !retry.next.After(time.Now()) {
			t.Fatalf("stop retry %+v, want delay %v", retry, want)
		}
		retry.next = time.Time{}
		a.stops[run.ID] = retry
	}
}
