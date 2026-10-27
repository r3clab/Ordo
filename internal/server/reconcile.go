package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/ssh"
	"github.com/r3clab/Ordo/internal/worker"
)

type host interface {
	Probe(ctx context.Context) (worker.Probe, error)
	Poll(ctx context.Context) (worker.Snapshot, error)
	EnsureImage(ctx context.Context, archive, id string) error
	EnsureInput(ctx context.Context, blob, digest string) error
	GPUBusy(ctx context.Context, gpus []pool.GPU) ([]string, error)
	Launch(ctx context.Context, l worker.Launch) (string, error)
	Stop(ctx context.Context, attempt string) error
	Inspect(ctx context.Context, attempt string) (podman.Inspection, error)
	Collect(ctx context.Context, attempt, dir string) error
	Remove(ctx context.Context, attempt string) error
}

// neverStarted bounds how long a created container may wait for the start that its launch performs.
const neverStarted = time.Minute

// view is one poll of a worker, shared by every job reconciled in the same pass.
type view struct {
	cfg        worker.Config
	host       host
	probe      worker.Probe
	boot       string
	now        time.Time
	containers map[string]podman.Container
	held       []pool.Allocation
	fault      string
}

func (s *Server) work(ctx context.Context, cfg worker.Config) {
	for {
		err := s.session(ctx, cfg)
		if ctx.Err() != nil {
			return
		}
		s.workerStatus(cfg.Host, nil, err)
		if !sleep(ctx, 2*time.Second) {
			return
		}
	}
}

func (s *Server) session(ctx context.Context, cfg worker.Config) error {
	m, err := worker.Dial(ctx, cfg, s.services)
	if err != nil {
		return err
	}
	defer func() { _ = m.Close() }()
	h := worker.New(cfg, m, s.id)
	probe, err := h.Probe(ctx)
	if err != nil {
		return err
	}
	for {
		err := s.pass(ctx, cfg, h, &probe)
		if errors.Is(err, ssh.ErrTransport) || ctx.Err() != nil {
			return cmp.Or(ctx.Err(), err)
		}
		if err != nil {
			s.workerStatus(cfg.Host, nil, err)
		}
		if !sleep(ctx, time.Second) {
			return ctx.Err()
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// pass reconciles every unfinished job against one poll of the worker. Only transport failures end
// it early; a job's own failure is recorded on the job.
func (s *Server) pass(ctx context.Context, cfg worker.Config, h host, probe *worker.Probe) error {
	snap, err := h.Poll(ctx)
	if err != nil {
		return err
	}
	if snap.BootID != probe.BootID {
		p, err := h.Probe(ctx)
		if err != nil {
			return err
		}
		*probe = p
	}
	v := &view{cfg: cfg, host: h, probe: *probe, boot: snap.BootID, now: snap.Now, containers: map[string]podman.Container{}}
	var foreign []string
	for _, c := range snap.Containers {
		if c.Server() != s.id {
			foreign = append(foreign, podman.Name(c.Attempt()))
			continue
		}
		v.containers[c.Attempt()] = c
	}
	if len(foreign) > 0 {
		v.fault = "worker runs Ordo containers of another server (" + strings.Join(foreign, ", ") + "); one server must own each worker account"
	}
	v.held = s.held(cfg.Host, v.containers)
	s.workerStatus(cfg.Host, &WorkerInfo{BootID: v.boot, Hardware: v.probe.Hardware, Held: v.held, Error: v.fault}, nil)
	for _, id := range s.unfinished() {
		s.mu.Lock()
		j := s.jobs[id].clone()
		available := !s.busy[id] && s.fault == nil && !j.State.Terminal()
		if available {
			s.busy[id] = true
		}
		s.mu.Unlock()
		if !available {
			continue
		}
		err := s.reconcile(ctx, j, v)
		s.mu.Lock()
		delete(s.busy, id)
		s.mu.Unlock()
		if errors.Is(err, ssh.ErrTransport) || ctx.Err() != nil {
			return cmp.Or(ctx.Err(), err)
		}
		if err != nil {
			s.note(id, err)
		}
	}
	return s.sweep(ctx, v)
}

// A container that outlives its attempt keeps its cores until it is removed.
func (s *Server) held(host string, containers map[string]podman.Container) []pool.Allocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var held []pool.Allocation
	for _, j := range s.jobs {
		for i, at := range j.Attempts {
			if _, present := containers[at.ID]; at.Worker == host && (j.live(i) || present) {
				held = append(held, at.Allocation)
			}
		}
	}
	return held
}

func (s *Server) note(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.State.Terminal() || j.Error == err.Error() {
		return
	}
	j = j.clone()
	j.Error = err.Error()
	_ = s.commitLocked(nil, j)
}

// block leaves a job for the operator: each cause needs a look, so only a cancel releases it, and
// nothing about it is retried meanwhile.
func (s *Server) block(j Job, reason string) error {
	s.refresh(&j)
	if j.CancelRequested {
		return s.finish(j, CancelFailure)
	}
	if j.State == Blocked {
		return nil
	}
	j.State = Blocked
	j.Error = reason + "; cancel the job to release it"
	return s.save(j)
}

func (s *Server) reconcile(ctx context.Context, j Job, v *view) error {
	switch j.State {
	case Queued:
		if j.stopKind() != "" {
			j.State = Cancelled
			return s.save(j)
		}
		ready, err := s.dependencies(&j)
		if err != nil || !ready {
			return err
		}
		if dispatched, err := s.dispatch(ctx, &j, v); err != nil || !dispatched {
			return err
		}
	case Blocked:
		return s.block(j, j.Error)
	}
	at := j.current()
	if at.Worker != v.cfg.Host {
		return nil
	}
	if at.MachineID != v.probe.Hardware.MachineID {
		return s.block(j, "worker identity changed")
	}
	s.refresh(&j)
	c, present := v.containers[at.ID]
	switch {
	case !present && at.ContainerID == "":
		return s.launch(ctx, j, v)
	case !present:
		return s.block(j, "container disappeared before its results were collected")
	case at.ContainerID != "" && at.ContainerID != c.ID:
		return s.block(j, "container identity changed")
	}
	changed := at.ContainerID == "" || j.State != Running
	at.ContainerID = c.ID
	switch {
	case c.State == podman.Exited:
		return s.collect(ctx, j, v)
	case v.boot != at.BootID:
		return s.abandon(ctx, j, v, "worker rebooted while the task ran")
	case c.StartedAt.IsZero() && v.now.Sub(c.Created.Time) > neverStarted:
		return s.abandon(ctx, j, v, "container was created but never started")
	}
	j.State = Running
	if at.StopRequested == "" {
		at.StopRequested, at.StopReason = j.stop(c.StartedAt.Time, v.now)
		changed = changed || at.StopRequested != ""
	}
	if changed {
		if err := s.save(j); err != nil {
			return err
		}
	}
	if at.StopRequested != "" && (c.State == podman.Running || c.State == podman.Paused) {
		return v.host.Stop(ctx, at.ID)
	}
	return nil
}

// stop measures the timeout on the worker's clock, which also stamped the container's start.
func (j Job) stop(started, now time.Time) (StopKind, string) {
	if kind := j.stopKind(); kind != "" {
		return kind, j.StopReason
	}
	if timeout := j.Spec.Timeout(); timeout > 0 && !started.IsZero() && now.Sub(started) >= timeout {
		return Timeout, ""
	}
	return "", ""
}

func (s *Server) dispatch(ctx context.Context, j *Job, v *view) (bool, error) {
	if v.fault != "" || !v.cfg.Carries(j.Spec.Tags) {
		return false, nil
	}
	hw := v.probe.Hardware
	allocation, err := pool.Plan(hw, j.Spec.Resources, v.held)
	if err == nil && j.Spec.Resources.GPUs > 0 {
		// A device busy outside Ordo would break the exclusive allocation the job asks for.
		var busy []string
		if busy, err = v.host.GPUBusy(ctx, hw.GPUs); err != nil {
			return false, err
		}
		allocation, err = pool.Plan(hw, j.Spec.Resources, append(slices.Clone(v.held), pool.Allocation{GPUs: busy}))
	}
	if errors.Is(err, pool.ErrCapacity) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := s.importOutputs(j); err != nil {
		return false, err
	}
	if err := v.host.EnsureImage(ctx, filepath.Join(s.images.Blobs, j.Archive), j.Spec.Image); err != nil {
		return false, err
	}
	for _, digest := range j.Spec.Inputs {
		if err := v.host.EnsureInput(ctx, filepath.Join(s.images.Blobs, digest), digest); err != nil {
			return false, err
		}
	}
	j.Attempts = append(j.Attempts, Attempt{ID: strings.ToLower(rand.Text()), Worker: v.cfg.Host, MachineID: hw.MachineID, BootID: v.boot, Allocation: allocation, CreatedAt: time.Now().UTC()})
	j.State = Dispatching
	if err := s.save(*j); err != nil {
		return false, err
	}
	v.held = append(v.held, allocation)
	return true, nil
}

// launch starts an attempt that no poll has shown yet. The attempt is durable before this runs, so a
// lost response is settled by the next poll, which adopts the container or launches again.
func (s *Server) launch(ctx context.Context, j Job, v *view) error {
	at := j.current()
	if j.stopKind() != "" {
		return s.finish(j, CancelFailure)
	}
	var secrets map[string]string
	if len(j.Spec.Secrets) > 0 {
		err := s.store.Get("secret", j.ID, &secrets)
		if errors.Is(err, sql.ErrNoRows) {
			at.Failure = InfrastructureFailure
			j.State = Failed
			j.Error = "job secrets are missing; resubmit it under a new ID"
			return s.save(j)
		}
		if err != nil {
			return err
		}
	}
	id, err := v.host.Launch(ctx, worker.Launch{Attempt: at.ID, Spec: j.Spec, Allocation: at.Allocation, Secrets: secrets})
	if errors.Is(err, ssh.ErrTransport) || ctx.Err() != nil {
		return cmp.Or(ctx.Err(), err)
	}
	if err != nil {
		// The name is taken when an earlier launch whose response was lost created the container.
		snap, pollErr := v.host.Poll(ctx)
		if pollErr != nil {
			return pollErr
		}
		if slices.ContainsFunc(snap.Containers, func(c podman.Container) bool { return c.Attempt() == at.ID }) {
			return nil
		}
		return s.abandon(ctx, j, v, "launch failed: "+err.Error())
	}
	at.ContainerID = id
	j.State = Running
	j.Error = ""
	return s.save(j)
}

func (s *Server) abandon(ctx context.Context, j Job, v *view, reason string) error {
	if err := v.host.Remove(ctx, j.current().ID); err != nil {
		return err
	}
	j.Error = reason
	return s.finish(j, InfrastructureFailure)
}

// sweep leaves alone the containers of attempts this server never made.
func (s *Server) sweep(ctx context.Context, v *view) error {
	s.mu.Lock()
	var done []string
	for _, j := range s.jobs {
		for i, at := range j.Attempts {
			if _, present := v.containers[at.ID]; present && !j.live(i) && !s.busy[j.ID] {
				done = append(done, at.ID)
			}
		}
	}
	s.mu.Unlock()
	for _, attempt := range done {
		if err := v.host.Remove(ctx, attempt); errors.Is(err, ssh.ErrTransport) {
			return err
		}
	}
	return nil
}
