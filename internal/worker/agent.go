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

func ValidNameSet(names []string) bool {
	sorted := slices.Sorted(slices.Values(names))
	for i, name := range sorted {
		if !ValidName(name) || (i > 0 && sorted[i-1] == name) {
			return false
		}
	}
	return true
}

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

func (s Spec) Normalize() Spec {
	if s.Match != nil {
		match := *s.Match
		match.Tags = slices.Sorted(slices.Values(match.Tags))
		s.Match = &match
	}
	s.Args = slices.Clone(s.Args)
	s.Inputs = maps.Clone(s.Inputs)
	s.Env = maps.Clone(s.Env)
	s.CapAdd = slices.Sorted(slices.Values(s.CapAdd))
	s.Secrets = slices.Sorted(slices.Values(s.Secrets))
	s.Services = slices.Sorted(slices.Values(s.Services))
	return s
}

func (s Spec) Validate(secrets map[string]string) error {
	if s.Resources.Cores <= 0 || s.Resources.MemoryMiB <= 0 || s.Resources.GPUs < 0 {
		return errors.New("positive cores/memory and a non-negative GPU count are required")
	}
	if s.TimeoutSeconds < 0 || s.TimeoutSeconds > MaxTimeoutSeconds {
		return fmt.Errorf("timeout must be 0..%d seconds", MaxTimeoutSeconds)
	}
	if s.Match != nil {
		if err := s.Match.Validate(s.Resources); err != nil {
			return err
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(s.Secrets)), slices.Sorted(maps.Keys(secrets))) {
		return errors.New("secret values must match the declared secret names")
	}
	if err := s.Sandbox.Validate(secrets); err != nil {
		return err
	}
	if !ValidNameSet(s.Services) {
		return errors.New("invalid or duplicate service name")
	}
	return nil
}

type Submission struct {
	Spec    Spec              `json:"spec"`
	Secrets map[string]string `json:"secrets,omitempty"`
}

type State string

const (
	Accepted             State = "accepted"
	Running              State = "running"
	Blocked              State = "blocked"
	Finished             State = "finished"
	InfrastructureFailed State = "infrastructure_failed"
	Cancelled            State = "cancelled"
)

func (s State) Terminal() bool { return s == Finished || s == InfrastructureFailed || s == Cancelled }

type StopKind string

const (
	Cancel  StopKind = "cancel"
	Stop    StopKind = "stop"
	Timeout StopKind = "timeout"
)

type Attempt struct {
	ID            string       `json:"id"`
	Spec          Spec         `json:"spec"`
	Allocation    Allocation   `json:"allocation"`
	Hardware      Hardware     `json:"hardware"`
	State         State        `json:"state"`
	Error         string       `json:"error,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	UpdatedAt     time.Time    `json:"updated_at"`
	Result        *task.Record `json:"result,omitempty"`
	StopRequested StopKind     `json:"stop_requested,omitempty"`
	StopReason    string       `json:"stop_reason,omitempty"`
}

// Collectable holds even when supervision failed after the run had durably finished.
func (a Attempt) Collectable() bool {
	return a.State.Terminal() && a.Result != nil && a.Result.State == task.Finished
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
	stops     map[string]stopRetry
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
	a := &Agent{cfg: cfg, info: Info{Protocol: protocolVersion, ID: ident.ID, Hardware: h}, store: store, attempts: map[string]Attempt{}, busy: map[string]bool{}, stops: map[string]stopRetry{}, cancelled: map[string]bool{}, ctx: child, cancel: cancel}
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
		case Accepted, Running, Blocked, Finished, InfrastructureFailed, Cancelled:
		default:
			return fail(fmt.Errorf("invalid saved attempt state %q", run.State))
		}
		if !run.State.Terminal() {
			if err := held.add(h, run); err != nil {
				return fail(err)
			}
		}
		a.attempts[run.ID] = run
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
	if !ValidName(id) || !task.ValidImageID(spec.Image) {
		return Attempt{}, fmt.Errorf("%w: valid attempt ID and immutable sha256 image ID are required", ErrInvalidRequest)
	}
	spec = spec.Normalize()
	if err := spec.Validate(secrets); err != nil {
		return Attempt{}, fmt.Errorf("%w: %s", ErrInvalidRequest, err)
	}
	var busy []string
	if spec.Resources.GPUs > 0 {
		// nvidia-smi pmon samples for about a second, so it runs unlocked, and only for a request that
		// would otherwise be admitted.
		a.mu.Lock()
		run, existing, err := a.plan(id, spec, nil)
		a.mu.Unlock()
		if err != nil || existing {
			return run, err
		}
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		busy, err = busyGPUs(ctx, a.info.Hardware.GPUs)
		cancel()
		if err != nil {
			return Attempt{}, err
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	run, existing, err := a.plan(id, spec, busy)
	if err != nil || existing {
		return run, err
	}
	if err := a.commit(run, secrets); err != nil {
		return Attempt{}, err
	}
	return a.attempts[id], nil
}

func (a *Agent) plan(id string, spec Spec, busy []string) (run Attempt, existing bool, err error) {
	if a.ctx.Err() != nil {
		return Attempt{}, false, a.ctx.Err()
	}
	if a.info.Error != "" {
		return Attempt{}, false, fmt.Errorf("%w: %s", ErrUnavailable, a.info.Error)
	}
	if a.cancelled[id] {
		return Attempt{}, false, fmt.Errorf("%w: attempt was cancelled before submission", ErrConflict)
	}
	for name, digest := range spec.Inputs {
		if !ValidName(name) || !content.Valid(digest) {
			return Attempt{}, false, fmt.Errorf("%w: invalid input name/digest", ErrInvalidRequest)
		}
		if info, err := os.Stat(a.treeDir(digest)); err != nil || !info.IsDir() {
			return Attempt{}, false, fmt.Errorf("%w: input %s is not cached", ErrInvalidRequest, name)
		}
	}
	if old, ok := a.attempts[id]; ok {
		left, _ := json.Marshal(old.Spec)
		right, _ := json.Marshal(spec)
		if !bytes.Equal(left, right) {
			return Attempt{}, false, fmt.Errorf("%w: attempt ID already belongs to a different request", ErrConflict)
		}
		return old, true, nil
	}
	if _, err := os.Lstat(a.runDir(id)); err == nil {
		return Attempt{}, false, fmt.Errorf("%w: unregistered run directory exists; refusing to reuse it", ErrConflict)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Attempt{}, false, err
	}
	if spec.Resources.GPUs > len(a.info.Hardware.GPUs) {
		return Attempt{}, false, ErrCapacity
	}
	held := a.held()
	if len(busy) > 0 {
		held = append(held, Allocation{GPUs: busy})
	}
	filtered, ok := matchedHardware(a.info.Hardware, spec.Match)
	if !ok {
		return Attempt{}, false, ErrCapacity
	}
	allocation, err := allocate(filtered, spec.Resources, held)
	if err != nil {
		return Attempt{}, false, err
	}
	now := time.Now().UTC()
	return Attempt{ID: id, Spec: spec, Allocation: allocation, Hardware: a.info.Hardware, State: Accepted, CreatedAt: now}, false, nil
}

func (a *Agent) held() []Allocation {
	var held []Allocation
	for _, run := range a.attempts {
		if !run.State.Terminal() {
			held = append(held, run.Allocation)
		}
	}
	return held
}

// latch closes admission after a failed write until a restart reconciles the database.
func (a *Agent) latch(err error) error {
	if err != nil {
		a.info.Error = err.Error()
	}
	return err
}

func (a *Agent) save(run Attempt) error { return a.commit(run, nil) }

// commit stores an attempt's secrets in the same transaction as the attempt, so they exist exactly
// while it is unfinished.
func (a *Agent) commit(run Attempt, secrets map[string]string) error {
	run.UpdatedAt = time.Now().UTC()
	err := a.store.Update(func(tx *state.Tx) error {
		if len(secrets) > 0 {
			if err := tx.Put("secret", run.ID, secrets); err != nil {
				return err
			}
		}
		if err := tx.Put("attempt", run.ID, run); err != nil {
			return err
		}
		if run.State.Terminal() && len(run.Spec.Secrets) > 0 {
			return tx.Delete("secret", run.ID)
		}
		return nil
	})
	if err != nil {
		return a.latch(err)
	}
	a.attempts[run.ID] = run
	return nil
}

func (a *Agent) runDir(id string) string { return filepath.Join(a.cfg.StateDir, "runs", id) }

func (a *Agent) treeDir(digest string) string {
	return filepath.Join(a.cfg.StateDir, "trees", digest)
}

func (a *Agent) serviceDir(name string) string {
	return filepath.Join(a.cfg.StateDir, "services", name)
}

func (a *Agent) launchPending() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx.Err() != nil || a.info.Error != "" {
		return
	}
	for id, run := range a.attempts {
		if run.State.Terminal() || a.busy[id] {
			continue
		}
		fresh := run.State == Accepted
		if fresh {
			run.State = Running
			if err := a.save(run); err != nil {
				return
			}
		}
		a.busy[id] = true
		a.wg.Go(func() { a.execute(run, fresh) })
	}
}

func (a *Agent) execute(run Attempt, fresh bool) {
	defer func() { a.mu.Lock(); delete(a.busy, run.ID); a.mu.Unlock() }()
	req, err := a.request(run)
	switch {
	case err != nil:
	case fresh:
		err = a.launch(req)
	default:
		err = resume(a.ctx, req)
	}
	// Shutdown detaches supervision, not the task or its reservation.
	if a.ctx.Err() != nil {
		return
	}
	state, result, err := a.settle(req, fresh, err)
	a.mu.Lock()
	defer a.mu.Unlock()
	run = a.attempts[run.ID]
	run.State, run.Error = state, ""
	if err != nil {
		run.Error = err.Error()
	}
	if result != nil {
		run.Result = result
	}
	if run.State == Finished && run.StopRequested == Cancel {
		run.State = Cancelled
	}
	_ = a.save(run)
}

func (a *Agent) request(run Attempt) (task.Request, error) {
	mounts := map[string]string{}
	for name, digest := range run.Spec.Inputs {
		mounts[name] = a.treeDir(digest)
	}
	services := map[string]string{}
	for _, name := range run.Spec.Services {
		services[name] = a.serviceDir(name)
	}
	req := task.Request{Image: run.Spec.Image, CPUs: task.FormatCPUs(run.Allocation.CPUs), MemoryMiB: run.Allocation.MemoryMiB, Args: run.Spec.Args, OutputDir: a.runDir(run.ID), ReservationID: ReservationID(a.info.ID, run.ID), Inputs: mounts, ExportOutputs: true, GPUs: run.Allocation.GPUs, CDISpecDir: filepath.Join(a.cfg.StateDir, "cdi"), Sandbox: run.Spec.Sandbox, Services: services}
	if len(run.Spec.Secrets) > 0 {
		var values map[string]string
		if err := a.store.Get("secret", run.ID, &values); err != nil {
			return req, fmt.Errorf("load secrets: %w", err)
		}
		req.Secrets = values
	}
	return req, nil
}

func (a *Agent) launch(req task.Request) error {
	for _, dir := range req.Services {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return task.Run(a.ctx, req)
}

func resume(ctx context.Context, req task.Request) error {
	r, err := task.ReadResult(req.OutputDir)
	if err != nil {
		return err
	}
	if !r.Request.Same(req) {
		return errors.New("run record does not match its reservation")
	}
	return task.Recover(ctx, req.OutputDir)
}

func (a *Agent) settle(req task.Request, fresh bool, err error) (State, *task.Record, error) {
	var exit *task.ExitError
	if err == nil || errors.As(err, &exit) {
		result, err := task.ReadResult(req.OutputDir)
		if err == nil && !result.Request.Same(req) {
			err = errors.New("record does not match its reservation")
		}
		if err != nil {
			return Blocked, nil, fmt.Errorf("cannot verify final record: %w", err)
		}
		return Finished, &result, nil
	}
	ctx, cancel := context.WithTimeout(a.ctx, 15*time.Second)
	defer cancel()
	result, status, probeErr := task.Status(ctx, req.OutputDir)
	switch {
	case probeErr == nil && result.Request.Same(req) && status.Gone(result):
		return InfrastructureFailed, &result, err
	case probeErr == nil && result.Request.Same(req):
		return Blocked, &result, err
	case fresh && errors.Is(probeErr, os.ErrNotExist):
		return InfrastructureFailed, nil, err
	}
	return Blocked, nil, err
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
