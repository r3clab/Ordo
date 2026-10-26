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

func (a *Agent) Cancel(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !identifier.MatchString(id) {
		return ErrInvalidRequest
	}
	if a.info.Error != "" {
		return ErrUnavailable
	}
	if run, ok := a.attempts[id]; ok {
		if run.Terminal() {
			return nil
		}
		run.StopRequested = "cancel"
		if run.State == "accepted" && !a.busy[id] {
			run.State = "cancelled"
		}
		return a.save(run)
	}
	if err := a.store.Put("cancel", id, cancellation{ID: id, Reason: "cancel"}); err != nil {
		a.info.Error = err.Error()
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
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			defer func() { a.mu.Lock(); delete(a.stopping, id); a.mu.Unlock() }()
			ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
			defer cancel()
			_ = task.Stop(ctx, a.runDir(id))
		}()
	}
}
