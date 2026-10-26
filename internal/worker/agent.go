package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/state"
	"github.com/r3clab/Ordo/internal/task"
)

var ErrCapacity = errors.New("worker has insufficient unreserved resources")
var ErrInvalidRequest = errors.New("invalid request")
var ErrConflict = errors.New("conflicting request")
var ErrUnavailable = errors.New("worker requires restart after a state write failure")

const NamePattern = `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`

var identifier = regexp.MustCompile(NamePattern)

const protocolVersion = 1

const MaxTimeoutSeconds = 365 * 24 * 60 * 60

func ValidName(name string) bool { return identifier.MatchString(name) }

func ReservationID(agentID, attemptID string) string { return agentID + "/" + attemptID }

type Config struct {
	StateDir  string   `json:"state_dir"`
	CPUs      string   `json:"cpus"`
	MemoryMiB int64    `json:"memory_mib"`
	GPUs      []string `json:"gpus,omitempty"`
}

type Spec struct {
	Image     string            `json:"image"`
	Args      []string          `json:"args,omitempty"`
	Resources Resources         `json:"resources"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	task.Sandbox
	Secrets        []string `json:"secrets,omitempty"`
	Services       []string `json:"services,omitempty"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty"`
	Match          *Match   `json:"match,omitempty"`
}

type Submission struct {
	Spec    Spec              `json:"spec"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

type Attempt struct {
	ID            string       `json:"id"`
	Spec          Spec         `json:"spec"`
	Allocation    Allocation   `json:"allocation"`
	Hardware      Hardware     `json:"hardware"`
	State         string       `json:"state"`
	Error         string       `json:"error,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	Result        *task.Record `json:"result,omitempty"`
	StopRequested string       `json:"stop_requested,omitempty"`
	StopReason    string       `json:"stop_reason,omitempty"`
}

func (a Attempt) Terminal() bool {
	return a.State == "finished" || a.State == "infrastructure_failed" || a.State == "cancelled"
}

type Info struct {
	Protocol int          `json:"protocol"`
	ID       string       `json:"id"`
	Hardware Hardware     `json:"hardware"`
	Held     []Allocation `json:"held"`
	Error    string       `json:"error,omitempty"`
}

type identity struct {
	ID        string `json:"id"`
	MachineID string `json:"machine_id"`
}

type Agent struct {
	mu        sync.Mutex
	cfg       Config
	info      Info
	store     *state.Store
	owner     *os.File
	attempts  map[string]Attempt
	busy      map[string]bool
	stopping  map[string]bool
	cancelled map[string]bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func Open(ctx context.Context, cfg Config) (*Agent, error) {
	if cfg.StateDir == "" {
		return nil, errors.New("--state is required")
	}
	root, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	cfg.StateDir = root
	owner, err := lockHost(fmt.Sprintf("/run/user/%d", os.Geteuid()), root)
	if err != nil {
		return nil, err
	}
	claimed := false
	defer func() {
		if !claimed {
			_ = owner.Close()
		}
	}()
	probe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := task.CheckHost(probe); err != nil {
		return nil, err
	}
	h, err := discover(cfg.CPUs, cfg.MemoryMiB)
	if err != nil {
		return nil, err
	}
	h.GPUs, err = discoverGPUs(probe, cfg.GPUs, cfg.StateDir)
	if err != nil {
		return nil, err
	}
	a, err := open(ctx, cfg, h)
	if err != nil {
		return nil, err
	}
	if err := bindHost(owner, root); err != nil {
		_ = a.Close()
		return nil, err
	}
	a.owner = owner
	claimed = true
	return a, nil
}

func open(ctx context.Context, cfg Config, h Hardware) (*Agent, error) {
	store, err := state.Open(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	var ident identity
	if err := store.Get("meta", "identity", &ident); errors.Is(err, sql.ErrNoRows) {
		ident = identity{ID: strings.ToLower(rand.Text()), MachineID: h.MachineID}
		if err := store.Put("meta", "identity", ident); err != nil {
			_ = store.Close()
			return nil, err
		}
	} else if err != nil {
		_ = store.Close()
		return nil, err
	}
	if ident.MachineID != h.MachineID {
		_ = store.Close()
		return nil, errors.New("state belongs to a different machine; refusing to adopt its reservations")
	}
	child, cancel := context.WithCancel(ctx)
	a := &Agent{cfg: cfg, info: Info{Protocol: protocolVersion, ID: ident.ID, Hardware: h}, store: store, attempts: map[string]Attempt{}, busy: map[string]bool{}, stopping: map[string]bool{}, cancelled: map[string]bool{}, ctx: child, cancel: cancel}
	fail := func(err error) (*Agent, error) { _ = a.Close(); return nil, err }
	tombstones, err := state.List[cancellation](store, "cancel")
	if err != nil {
		return fail(err)
	}
	for _, c := range tombstones {
		if !ValidName(c.ID) {
			return fail(errors.New("invalid cancellation record"))
		}
		a.cancelled[c.ID] = true
	}
	runs, err := state.List[Attempt](store, "attempt")
	if err != nil {
		return fail(err)
	}
	held := &reservations{cpus: map[int]bool{}, gpus: map[string]bool{}}
	for _, run := range runs {
		if !ValidName(run.ID) {
			return fail(errors.New("invalid saved attempt ID"))
		}
		switch run.State {
		case "accepted", "starting", "running", "blocked", "finished", "infrastructure_failed", "cancelled":
		default:
			return fail(fmt.Errorf("invalid saved attempt state %q", run.State))
		}
		if !run.Terminal() {
			if err := held.add(h, run); err != nil {
				return fail(err)
			}
		}
		a.attempts[run.ID] = run
	}
	// Secrets outlive their attempt when a crash or failed write falls between the two records.
	secrets, err := store.IDs("secret")
	if err != nil {
		return fail(err)
	}
	for _, id := range secrets {
		if run, ok := a.attempts[id]; !ok || run.Terminal() {
			if err := store.Delete("secret", id); err != nil {
				return fail(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(cfg.StateDir, "runs"), 0o700); err != nil {
		return fail(err)
	}
	return a, nil
}

type reservations struct {
	cpus      map[int]bool
	gpus      map[string]bool
	memoryMiB int64
}

func (held *reservations) add(h Hardware, run Attempt) error {
	selected, ok := matchedHardware(h, run.Spec.Match)
	if !ok {
		return errors.New("worker no longer matches a reserved attempt")
	}
	if len(run.Allocation.GPUs) != run.Spec.Resources.GPUs {
		return errors.New("saved GPU reservation count mismatch")
	}
	for _, id := range run.Allocation.GPUs {
		current := slices.IndexFunc(selected.GPUs, func(g GPU) bool { return g.UUID == id })
		previous := slices.IndexFunc(run.Hardware.GPUs, func(g GPU) bool { return g.UUID == id })
		if current < 0 || previous < 0 || held.gpus[id] {
			return errors.New("saved GPU reservations conflict or a device is missing")
		}
		now, old := selected.GPUs[current], run.Hardware.GPUs[previous]
		if now.Driver != old.Driver || now.Model != old.Model || now.MemoryMiB != old.MemoryMiB {
			return errors.New("reserved GPU hardware or driver changed")
		}
		held.gpus[id] = true
	}
	if run.Allocation.MemoryMiB <= 0 || run.Allocation.MemoryMiB > h.MemoryMiB-held.memoryMiB {
		return errors.New("saved reservations exceed configured memory")
	}
	held.memoryMiB += run.Allocation.MemoryMiB
	if len(run.Allocation.CPUs) == 0 {
		return errors.New("saved reservation has no CPUs")
	}
	for _, cpu := range run.Allocation.CPUs {
		if held.cpus[cpu] || !slices.ContainsFunc(h.Cores, func(core []int) bool { return slices.Contains(core, cpu) }) {
			return errors.New("saved CPU reservations conflict or fall outside the configured pool")
		}
		held.cpus[cpu] = true
	}
	for _, core := range h.Cores {
		count := 0
		for _, cpu := range core {
			if slices.Contains(run.Allocation.CPUs, cpu) {
				count++
			}
		}
		if count != 0 && count != len(core) {
			return errors.New("saved reservation no longer owns a complete physical core")
		}
	}
	return nil
}

func (a *Agent) Close() error {
	a.cancel()
	a.wg.Wait()
	a.mu.Lock()
	defer a.mu.Unlock()
	clear := a.info.Error == "" && len(a.held()) == 0
	err := a.store.Close()
	return errors.Join(err, releaseHost(a.owner, clear && err == nil))
}

func (a *Agent) Submit(id string, spec Spec, secrets map[string]string) (Attempt, error) {
	// nvidia-smi pmon samples for about a second; probing under a.mu would stall the agent.
	var busy []string
	var busyErr error
	if spec.Resources.GPUs > 0 {
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		busy, busyErr = busyGPUs(ctx, a.info.Hardware.GPUs)
		cancel()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil {
		return Attempt{}, a.ctx.Err()
	}
	if a.info.Error != "" {
		return Attempt{}, fmt.Errorf("%w: %s", ErrUnavailable, a.info.Error)
	}
	if !ValidName(id) || !task.ValidImageID(spec.Image) || spec.Resources.Cores <= 0 || spec.Resources.MemoryMiB <= 0 {
		return Attempt{}, fmt.Errorf("%w: valid attempt ID, immutable sha256 image ID, positive cores/memory are required", ErrInvalidRequest)
	}
	if spec.Resources.GPUs < 0 {
		return Attempt{}, fmt.Errorf("%w: GPU count cannot be negative", ErrInvalidRequest)
	}
	if spec.TimeoutSeconds < 0 || spec.TimeoutSeconds > MaxTimeoutSeconds {
		return Attempt{}, fmt.Errorf("%w: timeout must be 0..%d seconds", ErrInvalidRequest, MaxTimeoutSeconds)
	}
	if a.cancelled[id] {
		return Attempt{}, fmt.Errorf("%w: attempt was cancelled before submission", ErrConflict)
	}
	if spec.Match != nil {
		copy := *spec.Match
		if err := copy.Validate(spec.Resources); err != nil {
			return Attempt{}, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
		}
		spec.Match = &copy
	}
	spec.Args = slices.Clone(spec.Args)
	spec.Inputs = maps.Clone(spec.Inputs)
	spec.Env = maps.Clone(spec.Env)
	spec.CapAdd = slices.Sorted(slices.Values(spec.CapAdd))
	spec.Secrets = slices.Sorted(slices.Values(spec.Secrets))
	spec.Services = slices.Sorted(slices.Values(spec.Services))
	if !slices.Equal(spec.Secrets, slices.Sorted(maps.Keys(secrets))) {
		return Attempt{}, fmt.Errorf("%w: secret values must match the declared secret names", ErrInvalidRequest)
	}
	if err := spec.Validate(secrets); err != nil {
		return Attempt{}, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}
	for i, name := range spec.Services {
		if !ValidName(name) || (i > 0 && spec.Services[i-1] == name) {
			return Attempt{}, fmt.Errorf("%w: invalid or duplicate service name", ErrInvalidRequest)
		}
	}
	for name, digest := range spec.Inputs {
		if !ValidName(name) || !content.Valid(digest) {
			return Attempt{}, fmt.Errorf("%w: invalid input name/digest", ErrInvalidRequest)
		}
		if info, err := os.Stat(filepath.Join(a.cfg.StateDir, "trees", digest)); err != nil || !info.IsDir() {
			return Attempt{}, fmt.Errorf("%w: input %s is not cached", ErrInvalidRequest, name)
		}
	}
	if old, ok := a.attempts[id]; ok {
		left, _ := json.Marshal(old.Spec)
		right, _ := json.Marshal(spec)
		if !bytes.Equal(left, right) {
			return Attempt{}, fmt.Errorf("%w: attempt ID already belongs to a different request", ErrConflict)
		}
		return old, nil
	}
	if _, err := os.Lstat(a.runDir(id)); err == nil {
		return Attempt{}, fmt.Errorf("%w: unregistered run directory exists; refusing to reuse it", ErrConflict)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Attempt{}, err
	}
	held := a.held()
	if spec.Resources.GPUs > len(a.info.Hardware.GPUs) {
		return Attempt{}, ErrCapacity
	}
	if spec.Resources.GPUs > 0 {
		if busyErr != nil {
			return Attempt{}, busyErr
		}
		held = append(held, Allocation{GPUs: busy})
	}
	filtered, ok := matchedHardware(a.info.Hardware, spec.Match)
	if !ok {
		return Attempt{}, ErrCapacity
	}
	allocation, err := allocate(filtered, spec.Resources, held)
	if err != nil {
		return Attempt{}, err
	}
	now := time.Now().UTC()
	run := Attempt{ID: id, Spec: spec, Allocation: allocation, Hardware: a.info.Hardware, State: "accepted", CreatedAt: now, UpdatedAt: now}
	if len(secrets) > 0 {
		if err := a.put("secret", id, secrets); err != nil {
			return Attempt{}, err
		}
	}
	if err := a.save(run); err != nil {
		return Attempt{}, err
	}
	return run, nil
}

func (a *Agent) held() []Allocation {
	var held []Allocation
	for _, run := range a.attempts {
		if !run.Terminal() {
			held = append(held, run.Allocation)
		}
	}
	return held
}

func (a *Agent) put(kind, id string, value any) error { return a.latch(a.store.Put(kind, id, value)) }

func (a *Agent) latch(err error) error {
	if err != nil {
		a.info.Error = err.Error()
	}
	return err
}

func (a *Agent) save(run Attempt) error {
	if old, ok := a.attempts[run.ID]; ok && old.StopRequested != "" {
		run.StopRequested = old.StopRequested
		run.StopReason = old.StopReason
	}
	if run.State == "finished" && run.StopRequested == "cancel" {
		run.State = "cancelled"
	}
	run.UpdatedAt = time.Now().UTC()
	if err := a.put("attempt", run.ID, run); err != nil {
		return err
	}
	a.attempts[run.ID] = run
	if run.Terminal() && len(run.Spec.Secrets) > 0 {
		return a.latch(a.store.Delete("secret", run.ID))
	}
	return nil
}

func (a *Agent) runDir(id string) string { return filepath.Join(a.cfg.StateDir, "runs", id) }

func (a *Agent) launchPending() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil || a.info.Error != "" {
		return
	}
	for id, run := range a.attempts {
		if run.Terminal() || a.busy[id] {
			continue
		}
		fresh := run.State == "accepted"
		if fresh {
			run.State = "running"
			if err := a.save(run); err != nil {
				return
			}
		}
		a.busy[id] = true
		a.wg.Add(1)
		go a.execute(run, fresh)
	}
}

func (a *Agent) execute(run Attempt, fresh bool) {
	defer a.wg.Done()
	defer func() { a.mu.Lock(); delete(a.busy, run.ID); a.mu.Unlock() }()
	dir := a.runDir(run.ID)
	mounts := map[string]string{}
	for name, digest := range run.Spec.Inputs {
		mounts[name] = filepath.Join(a.cfg.StateDir, "trees", digest)
	}
	services := map[string]string{}
	for _, name := range run.Spec.Services {
		services[name] = a.serviceDir(name)
	}
	req := task.Request{Image: run.Spec.Image, CPUs: cpuString(run.Allocation.CPUs), MemoryMiB: run.Allocation.MemoryMiB, Args: run.Spec.Args, OutputDir: dir, ReservationID: ReservationID(a.info.ID, run.ID), Inputs: mounts, ExportOutputs: true, GPUs: run.Allocation.GPUs, CDISpecDir: filepath.Join(a.cfg.StateDir, "cdi"), Sandbox: run.Spec.Sandbox, Services: services}
	matches := func(r task.Record) bool {
		got := r.Request
		return got.ReservationID == req.ReservationID && got.Image == req.Image && got.CPUs == req.CPUs && got.MemoryMiB == req.MemoryMiB && slices.Equal(got.Args, req.Args) && maps.Equal(got.Inputs, req.Inputs) && slices.Equal(got.GPUs, req.GPUs) &&
			maps.Equal(got.Env, req.Env) && got.Network == req.Network && slices.Equal(got.CapAdd, req.CapAdd) && slices.Equal(got.Secrets.Names(), run.Spec.Secrets) && maps.Equal(got.Services, req.Services)
	}
	var err error
	if fresh {
		err = a.prepareLaunch(run, &req)
		if err == nil {
			err = task.Run(a.ctx, req)
		}
	} else {
		var r task.Record
		r, err = task.ReadResult(dir)
		if err == nil && !matches(r) {
			err = errors.New("run record does not match its reservation")
		}
		if err == nil {
			err = task.Recover(a.ctx, dir)
		}
	}
	if a.ctx.Err() != nil {
		return
	} // Shutdown detaches supervision, not the task or its reservation.
	var exit *task.ExitError
	if err == nil || errors.As(err, &exit) {
		result, readErr := task.ReadResult(dir)
		if readErr == nil && matches(result) {
			run.State = "finished"
			run.Error = ""
			run.Result = &result
		} else {
			run.State = "blocked"
			run.Error = fmt.Sprintf("cannot verify final record: %v", readErr)
		}
	} else {
		run.State = "blocked"
		run.Error = err.Error()
		ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
		result, status, probeErr := task.Status(ctx, dir)
		cancel()
		if probeErr == nil && matches(result) {
			run.Result = &result
			// A missing ID after an interrupted launch is not proof that no child can still create it.
			if status == "unstarted" || status == "removed" || (status == "missing" && result.ContainerID != "") {
				run.State = "infrastructure_failed"
			}
		} else if fresh && errors.Is(probeErr, os.ErrNotExist) {
			run.State = "infrastructure_failed"
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	// A failed write latches admission closed until restart reconciles the database.
	_ = a.save(run)
}

func (a *Agent) serviceDir(name string) string {
	return filepath.Join(a.cfg.StateDir, "services", name)
}

func (a *Agent) prepareLaunch(run Attempt, req *task.Request) error {
	for _, dir := range req.Services {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if len(run.Spec.Secrets) == 0 {
		return nil
	}
	var values map[string]string
	if err := a.store.Get("secret", run.ID, &values); err != nil {
		return fmt.Errorf("load secrets: %w", err)
	}
	req.Secrets = values
	return nil
}

func (a *Agent) Serve() error {
	listener, err := ListenUnix(Socket(a.cfg.StateDir))
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	server := &http.Server{Handler: a.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	a.launchPending()
	for {
		select {
		case <-a.ctx.Done():
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := server.Shutdown(ctx)
			cancel()
			return err
		case err := <-done:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			a.enforceStops()
			a.launchPending()
		}
	}
}
