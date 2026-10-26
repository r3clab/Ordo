package worker

import (
	"context"
	"time"

	"github.com/r3clab/Ordo/internal/task"
)

type cancellation struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

const MaxReasonBytes = 1024

const maxStopDelay = 30 * time.Second

// A stop is retried with backoff: it can precede the container it targets, and the attempt ends only
// once its results are archived.
type stopRetry struct {
	active bool
	next   time.Time
	delay  time.Duration
}

func (a *Agent) Cancel(id, reason string) error { return a.requestStop(id, Cancel, reason) }

func (a *Agent) Stop(id, reason string) error { return a.requestStop(id, Stop, reason) }

func (a *Agent) requestStop(id string, kind StopKind, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !ValidName(id) || len(reason) > MaxReasonBytes {
		return ErrInvalidRequest
	}
	if a.info.Error != "" {
		return ErrUnavailable
	}
	if run, ok := a.attempts[id]; ok {
		if run.State.Terminal() || run.StopRequested != "" {
			return nil
		}
		run.StopRequested = kind
		run.StopReason = reason
		if a.unlaunched(run) {
			run.State = Cancelled
		}
		return a.save(run)
	}
	if err := a.latch(a.store.Put("cancel", id, cancellation{ID: id, Reason: reason})); err != nil {
		return err
	}
	a.cancelled[id] = true
	return nil
}

func (a *Agent) unlaunched(run Attempt) bool { return run.State == Accepted && !a.busy[run.ID] }

func (a *Agent) enforceStops() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil || a.info.Error != "" {
		return
	}
	now := time.Now()
	for id, run := range a.attempts {
		if run.State.Terminal() {
			delete(a.stops, id)
			continue
		}
		if run.Spec.TimeoutSeconds > 0 && run.StopRequested == "" && now.Sub(run.CreatedAt) >= time.Duration(run.Spec.TimeoutSeconds)*time.Second {
			run.StopRequested = Timeout
			if err := a.save(run); err != nil {
				return
			}
		}
		retry := a.stops[id]
		if run.StopRequested == "" || retry.active || now.Before(retry.next) {
			continue
		}
		if a.unlaunched(run) {
			run.State = Cancelled
			if err := a.save(run); err != nil {
				return
			}
			continue
		}
		a.stops[id] = stopRetry{active: true, delay: min(max(2*retry.delay, time.Second), maxStopDelay)}
		a.wg.Go(func() {
			ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
			defer cancel()
			_ = task.Stop(ctx, a.runDir(id))
			a.mu.Lock()
			defer a.mu.Unlock()
			retry := a.stops[id]
			a.stops[id] = stopRetry{next: time.Now().Add(retry.delay), delay: retry.delay}
		})
	}
}
