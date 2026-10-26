package worker

import (
	"context"
	"github.com/r3clab/Ordo/internal/task"
	"time"
)

type cancellation struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

const MaxReasonBytes = 1024

func (a *Agent) Cancel(id, reason string) error { return a.requestStop(id, "cancel", reason) }

func (a *Agent) Stop(id, reason string) error { return a.requestStop(id, "stop", reason) }

func (a *Agent) requestStop(id, kind, reason string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !ValidName(id) || len(reason) > MaxReasonBytes {
		return ErrInvalidRequest
	}
	if a.info.Error != "" {
		return ErrUnavailable
	}
	if run, ok := a.attempts[id]; ok {
		if run.Terminal() || run.StopRequested != "" {
			return nil
		}
		run.StopRequested = kind
		run.StopReason = reason
		if run.State == "accepted" && !a.busy[id] {
			run.State = "cancelled"
		}
		return a.save(run)
	}
	if err := a.put("cancel", id, cancellation{ID: id, Reason: reason}); err != nil {
		return err
	}
	a.cancelled[id] = true
	return nil
}

func (a *Agent) enforceStops() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil || a.info.Error != "" {
		return
	}
	for id, run := range a.attempts {
		if run.Terminal() {
			continue
		}
		if run.Spec.TimeoutSeconds > 0 && run.StopRequested == "" && time.Since(run.CreatedAt) >= time.Duration(run.Spec.TimeoutSeconds)*time.Second {
			run.StopRequested = "timeout"
			if err := a.save(run); err != nil {
				return
			}
		}
		if run.StopRequested == "" || a.stopping[id] {
			continue
		}
		if run.State == "accepted" && !a.busy[id] {
			run.State = "cancelled"
			if err := a.save(run); err != nil {
				return
			}
			continue
		}
		a.stopping[id] = true
		a.wg.Go(func() {
			defer func() { a.mu.Lock(); delete(a.stopping, id); a.mu.Unlock() }()
			ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
			defer cancel()
			_ = task.Stop(ctx, a.runDir(id))
		})
	}
}
