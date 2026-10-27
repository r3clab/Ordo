package server

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	Seed(ctx context.Context, attempt, prev, archive string) error
	GPUBusy(ctx context.Context, gpus []pool.GPU) ([]string, error)
	Launch(ctx context.Context, l worker.Launch) (string, error)
	Stop(ctx context.Context, attempt string) error
	Inspect(ctx context.Context, attempt string) (podman.Inspection, error)
	Collect(ctx context.Context, attempt, dir string) error
	Remove(ctx context.Context, attempt string) error
	Release(ctx context.Context, attempt string) error
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
	runs       []string
	held       []pool.Allocation
	fault      string
	gpusBusy   []string
	gpusPolled bool
	step       func() error
}

// later hands reconcile's slow last step, a launch with its uploads or a collection, to the session.
func (v *view) later(step func() error) error {
	v.step = step
	return nil
}

// steps runs a session's slow steps beside its passes, so one transfer does not hold up the worker's
// other jobs. The first one that ends the session cancels it.
type steps struct {
	wg     sync.WaitGroup
	cancel context.CancelCauseFunc
}

func (b *steps) run(ctx context.Context, s *Server, id string, step func() error) {
	b.wg.Go(func() {
		err := step()
		s.mu.Lock()
		delete(s.busy, id)
		s.mu.Unlock()
		if end := fatal(ctx, err); end != nil {
			b.cancel(end)
		} else if err != nil {
			s.note(id, err)
		}
	})
}

// busyGPUs samples once per pass: each sample is an SSH round trip and about a second of nvidia-smi.
func (v *view) busyGPUs(ctx context.Context) ([]string, error) {
	if !v.gpusPolled {
		busy, err := v.host.GPUBusy(ctx, v.probe.Hardware.GPUs)
		if err != nil {
			return nil, err
		}
		v.gpusBusy, v.gpusPolled = busy, true
	}
	return v.gpusBusy, nil
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
	ctx, cancel := context.WithCancelCause(ctx)
	bg := &steps{cancel: cancel}
	defer func() {
		cancel(nil)
		bg.wg.Wait()
	}()
	h := worker.New(cfg, m, s.id)
	probe, err := h.Probe(ctx)
	if err != nil {
		return err
	}
	for {
		err := s.pass(ctx, cfg, h, &probe, bg)
		if end := fatal(ctx, err); end != nil {
			return end
		}
		if err != nil {
			s.workerStatus(cfg.Host, nil, err)
		}
		if !sleep(ctx, time.Second) {
			return context.Cause(ctx)
		}
	}
}

func fatal(ctx context.Context, err error) error {
	if errors.Is(err, ssh.ErrTransport) || ctx.Err() != nil {
		return cmp.Or(context.Cause(ctx), err)
	}
	return nil
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
func (s *Server) pass(ctx context.Context, cfg worker.Config, h host, probe *worker.Probe, bg *steps) error {
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
	v := &view{cfg: cfg, host: h, probe: *probe, boot: snap.BootID, now: snap.Now, containers: map[string]podman.Container{}, runs: snap.Runs}
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
	if v.held, err = s.held(cfg.Host, v.containers); err != nil {
		return err
	}
	s.workerStatus(cfg.Host, &WorkerInfo{BootID: v.boot, Hardware: v.probe.Hardware, Held: v.held, Error: v.fault}, nil)
	for _, id := range s.unfinished() {
		s.mu.Lock()
		j, unfinished := s.jobs[id]
		available := unfinished && !s.busy[id] && s.fault == nil
		if available {
			s.busy[id] = true
		}
		s.mu.Unlock()
		if !available {
			continue
		}
		err := s.reconcile(ctx, j.clone(), v)
		if step := v.step; step != nil {
			v.step = nil
			bg.run(ctx, s, id, step)
			continue
		}
		s.mu.Lock()
		delete(s.busy, id)
		s.mu.Unlock()
		if end := fatal(ctx, err); end != nil {
			return end
		}
		if err != nil {
			s.note(id, err)
		}
	}
	return s.sweep(ctx, v)
}

// A container that outlives its attempt keeps its cores until it is removed. Such a container is rare,
// as sweep removes it, so a finished job's attempts are read back from the store only then.
func (s *Server) held(host string, containers map[string]podman.Container) ([]pool.Allocation, error) {
	s.mu.Lock()
	var held []pool.Allocation
	for _, j := range s.jobs {
		for i, at := range j.Attempts {
			if _, present := containers[at.ID]; at.Worker == host && (j.live(i) || present) {
				held = append(held, at.Allocation)
			}
		}
	}
	finished := map[string]bool{}
	for attempt := range containers {
		if id, ok := s.attempts[attempt]; ok {
			if _, unfinished := s.jobs[id]; !unfinished {
				finished[id] = true
			}
		}
	}
	s.mu.Unlock()
	for id := range finished {
		j, _, err := s.job(id)
		if err != nil {
			return nil, err
		}
		for _, at := range j.Attempts {
			if _, present := containers[at.ID]; present && at.Worker == host {
				held = append(held, at.Allocation)
			}
		}
	}
	return held, nil
}

func (s *Server) note(id string, err error) {
	s.write.Lock()
	defer s.write.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.Error == err.Error() {
		return
	}
	j = j.clone()
	j.Error = err.Error()
	_ = s.commit(j)
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
		if dispatched, err := s.dispatch(ctx, &j, v); err != nil || !dispatched {
			return err
		}
	case Blocked:
		return s.block(j, j.Error)
	case Delivering:
		return v.later(func() error { return s.deliver(j) })
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
		return v.later(func() error { return s.launch(ctx, j, v) })
	case !present:
		return s.block(j, "container disappeared before its results were collected")
	case at.ContainerID != "" && at.ContainerID != c.ID:
		return s.block(j, "container identity changed")
	}
	changed := at.ContainerID == "" || j.State != Running
	at.ContainerID = c.ID
	switch {
	case c.State == podman.Exited:
		return v.later(func() error { return s.collect(ctx, j, v) })
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
	if !started.IsZero() && j.spec().TimedOut(started, now) {
		return Timeout, ""
	}
	return "", ""
}

// stepAffinity is how long a step waits for the worker that ran the step before it, which can start
// it from the outputs it kept instead of an upload.
const stepAffinity = time.Minute

func (s *Server) dispatch(ctx context.Context, j *Job, v *view) (bool, error) {
	resources := j.spec().Resources
	if v.fault != "" || !v.cfg.Carries(j.spec().Tags) {
		return false, nil
	}
	if prev := j.checkpoint(); prev != nil && prev.Worker != v.cfg.Host && time.Since(j.ReadyAt) < stepAffinity {
		return false, nil
	}
	hw := v.probe.Hardware
	allocation, err := pool.Plan(hw, resources, v.held)
	if err == nil && resources.GPUs > 0 {
		// A device busy outside Ordo would break the exclusive allocation the job asks for.
		var busy []string
		if busy, err = v.busyGPUs(ctx); err != nil {
			return false, err
		}
		allocation, err = pool.Plan(hw, resources, append(slices.Clone(v.held), pool.Allocation{GPUs: busy}))
	}
	if errors.Is(err, pool.ErrCapacity) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	j.Attempts = append(j.Attempts, Attempt{ID: newID(), Step: j.Step, Worker: v.cfg.Host, MachineID: hw.MachineID, BootID: v.boot, Allocation: allocation, CreatedAt: time.Now().UTC()})
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
	if err := s.upload(ctx, j, v); err != nil {
		if end := fatal(ctx, err); end != nil {
			return end
		}
		// Nothing ran yet, so the job goes back to the queue, where another worker may take it.
		_ = v.host.Remove(ctx, at.ID)
		j.Attempts = j.Attempts[:len(j.Attempts)-1]
		j.State = Queued
		j.Error = err.Error()
		return s.save(j)
	}
	sp := j.spec()
	secrets := make(map[string]string, len(sp.Secrets))
	for _, name := range sp.Secrets {
		secrets[name] = s.secrets[name]
	}
	id, err := v.host.Launch(ctx, worker.Launch{Attempt: at.ID, Spec: sp, Allocation: at.Allocation, Secrets: secrets})
	if end := fatal(ctx, err); end != nil {
		return end
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

func (s *Server) upload(ctx context.Context, j Job, v *view) error {
	stage := j.Steps[j.Step]
	if err := v.host.EnsureImage(ctx, filepath.Join(s.images.Blobs, stage.Archive), stage.Spec.Image); err != nil {
		return err
	}
	for _, digest := range stage.Spec.Inputs {
		if err := v.host.EnsureInput(ctx, filepath.Join(s.images.Blobs, digest), digest); err != nil {
			return err
		}
	}
	if prev := j.checkpoint(); prev != nil {
		return v.host.Seed(ctx, j.current().ID, prev.ID, filepath.Join(prev.ResultDir, OutputsArchive))
	}
	return nil
}

func (s *Server) abandon(ctx context.Context, j Job, v *view, reason string) error {
	if err := v.host.Remove(ctx, j.current().ID); err != nil {
		return err
	}
	j.Error = reason
	return s.finish(j, InfrastructureFailure)
}

// sweep removes what finished attempts left on the worker, their containers and run directories,
// except the outputs a step has yet to start from. It leaves alone the attempts this server never made.
func (s *Server) sweep(ctx context.Context, v *view) error {
	s.mu.Lock()
	var done []string
	for _, attempt := range slices.Concat(slices.Collect(maps.Keys(v.containers)), v.runs) {
		id, ok := s.attempts[attempt]
		if !ok || s.busy[id] || slices.Contains(done, attempt) {
			continue
		}
		if j, unfinished := s.jobs[id]; unfinished && (j.current().ID == attempt && j.live(len(j.Attempts)-1) || j.kept() == attempt) {
			continue
		}
		done = append(done, attempt)
	}
	s.mu.Unlock()
	for _, attempt := range done {
		if err := v.host.Remove(ctx, attempt); errors.Is(err, ssh.ErrTransport) {
			return err
		}
	}
	return nil
}
