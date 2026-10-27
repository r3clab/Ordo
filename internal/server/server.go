package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
	"github.com/r3clab/Ordo/internal/state"
	"github.com/r3clab/Ordo/internal/worker"
)

// Step is one container of a job. A job's steps run in order, and each starts with what the earlier
// ones left in /outputs.
type Step struct {
	Name  string       `json:"name,omitempty"`
	Image image.Source `json:"image"`
	spec.Task
}

// Request is a job that writes its results into Out. A job of one step may give that step's fields at
// the top level instead of in Steps.
type Request struct {
	Out    string            `json:"out"`
	Inputs map[string]string `json:"inputs,omitempty"`
	Steps  []Step            `json:"steps,omitempty"`
	Step
	Retry RetryPolicy `json:"retry,omitempty"`
}

func (r Request) steps() []Step {
	if len(r.Steps) == 0 {
		return []Step{r.Step}
	}
	return r.Steps
}

type Failure string

const (
	TaskFailure           Failure = "task"
	InfrastructureFailure Failure = "infrastructure"
	CancelFailure         Failure = "cancel"
)

type StopKind string

const (
	Cancel  StopKind = "cancel"
	Stop    StopKind = "stop"
	Timeout StopKind = "timeout"
)

type Result struct {
	ExitCode      int       `json:"exit_code"`
	OOMKilled     bool      `json:"oom_killed,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	OutputsSHA256 string    `json:"outputs_sha256"`
}

// Attempt is durable before its container is launched, so a restarted server finds the container by
// its ID instead of launching a second one.
type Attempt struct {
	ID            string          `json:"id"`
	Step          int             `json:"step"`
	Worker        string          `json:"worker"`
	MachineID     string          `json:"machine_id"`
	BootID        string          `json:"boot_id"`
	Allocation    pool.Allocation `json:"allocation"`
	CreatedAt     time.Time       `json:"created_at"`
	ContainerID   string          `json:"container_id,omitempty"`
	StopRequested StopKind        `json:"stop_requested,omitempty"`
	StopReason    string          `json:"stop_reason,omitempty"`
	Result        *Result         `json:"result,omitempty"`
	ResultDir     string          `json:"result_dir,omitempty"`
	Failure       Failure         `json:"failure,omitempty"`
}

type State string

const (
	Queued      State = "queued"
	Dispatching State = "dispatching"
	Running     State = "running"
	Blocked     State = "blocked"
	Collecting  State = "collecting"
	Delivering  State = "delivering"
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	Cancelled   State = "cancelled"
	Skipped     State = "skipped"
)

func (s State) Terminal() bool {
	return s == Succeeded || s == Failed || s == Cancelled || s == Skipped
}

// Stage is a step as its job runs it, with the image and inputs resolved to content.
type Stage struct {
	Name        string        `json:"name,omitempty"`
	Spec        spec.Spec     `json:"spec"`
	Archive     string        `json:"archive"`
	ImageOrigin *image.Origin `json:"image_origin,omitempty"`
}

type Job struct {
	ID      string    `json:"id"`
	Out     string    `json:"out"`
	Steps   []Stage   `json:"steps"`
	Step    int       `json:"step"`
	ReadyAt time.Time `json:"ready_at,omitzero"`
	// Avoid is the worker the last attempt failed on, which gives the others a head start at the retry.
	Avoid           string      `json:"avoid,omitempty"`
	State           State       `json:"state"`
	Error           string      `json:"error,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
	FinishedAt      time.Time   `json:"finished_at,omitzero"`
	Attempts        []Attempt   `json:"attempts,omitempty"`
	Key             string      `json:"submission_key,omitempty"`
	Retry           RetryPolicy `json:"retry,omitempty"`
	CancelRequested bool        `json:"cancel_requested,omitempty"`
	StopRequested   bool        `json:"stop_requested,omitempty"`
	StopReason      string      `json:"stop_reason,omitempty"`
	// Inputs keeps each input's source. Waiting names those that lie in the Out of the jobs in After:
	// they are snapshotted once those jobs succeed, and the job runs only then.
	Inputs  map[string]string `json:"inputs,omitempty"`
	Waiting []string          `json:"waiting,omitempty"`
	After   []string          `json:"after,omitempty"`
}

func (j Job) spec() spec.Spec { return j.Steps[j.Step].Spec }

// Summary is how GET /v1/jobs lists a job; GET /v1/jobs/{id} returns all of it.
type Summary struct {
	ID              string         `json:"id"`
	Out             string         `json:"out"`
	State           State          `json:"state"`
	Step            int            `json:"step"`
	Steps           int            `json:"steps"`
	Image           string         `json:"image"`
	Resources       spec.Resources `json:"resources"`
	Worker          string         `json:"worker,omitempty"`
	Attempts        int            `json:"attempts,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	FinishedAt      time.Time      `json:"finished_at,omitzero"`
	Error           string         `json:"error,omitempty"`
	CancelRequested bool           `json:"cancel_requested,omitempty"`
	After           []string       `json:"after,omitempty"`
	// Pending says, for each connected worker that could run a queued job, why it has not yet.
	Pending map[string]string `json:"pending,omitempty"`
}

func (j Job) summary() Summary {
	s := Summary{ID: j.ID, Out: j.Out, State: j.State, Step: j.Step, Steps: len(j.Steps), Attempts: len(j.Attempts), CreatedAt: j.CreatedAt, FinishedAt: j.FinishedAt, Error: j.Error, CancelRequested: j.CancelRequested, After: j.After}
	if j.Step < len(j.Steps) {
		s.Image, s.Resources = j.spec().Image, j.spec().Resources
	}
	if len(j.Attempts) > 0 {
		s.Worker = j.current().Worker
	}
	return s
}

// record is what stays in memory of every job, finished or not.
type record struct {
	Summary
	key string
}

type Server struct {
	// write serializes changes to jobs, records, attempts and the store, so mu guards only reading and
	// installing them and is never held across a commit. A holder of write may read them without mu.
	write sync.Mutex
	mu    sync.Mutex
	// staging is held for reading while blobs are stored for jobs not yet committed, which pruneBlobs
	// cannot yet see as in use.
	staging    sync.RWMutex
	root       string
	store      *state.Store
	id         string
	workers    []worker.Config
	services   map[string]string
	secretsDir string
	sshConfig  string
	// jobs holds only unfinished jobs, and outs maps their Out to their ID; a finished job is read back
	// from the store.
	jobs     map[string]Job
	outs     map[string]string
	records  map[string]record
	attempts map[string]string
	busy     map[string]bool
	fault    error
	nodes    map[string]WorkerStatus
	pending  map[string]map[string]string
	images   *image.Resolver
}

type Config struct {
	Root     string
	Workers  []worker.Config
	Services map[string]string
	Tools    image.Tools
	// SecretsDir holds one file per secret, named after it.
	SecretsDir string
	SSHConfig  string
	// Home finds a worker's home for its default state directory; nil uses ssh.Home with SSHConfig.
	Home func(ctx context.Context, host string) (string, error)
}

func Open(c Config) (_ *Server, err error) {
	if len(c.Workers) == 0 {
		return nil, errors.New("configure at least one worker")
	}
	root, err := filepath.Abs(c.Root)
	if err != nil {
		return nil, err
	}
	db, err := state.Open(root)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	home := c.Home
	if home == nil {
		home = func(ctx context.Context, host string) (string, error) { return ssh.Home(ctx, host, c.SSHConfig) }
	}
	workers, err := stateDirs(db, c.Workers, home)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, w := range workers {
		if seen[w.Host] {
			return nil, errors.New("worker SSH aliases must be unique")
		}
		if err := w.Validate(); err != nil {
			return nil, fmt.Errorf("worker %s: %w", w.Host, err)
		}
		seen[w.Host] = true
		for name, address := range c.Services {
			if _, err := w.ServiceForward(name, address); err != nil {
				return nil, err
			}
		}
	}
	s := &Server{root: root, store: db, workers: workers, services: maps.Clone(c.Services), secretsDir: c.SecretsDir, sshConfig: c.SSHConfig, jobs: map[string]Job{}, outs: map[string]string{}, records: map[string]record{}, attempts: map[string]string{}, busy: map[string]bool{}, nodes: map[string]WorkerStatus{}, pending: map[string]map[string]string{}}
	// Containers carry this ID, so a worker account shared with another server is detected.
	err = db.Get("meta", "server", &s.id)
	if errors.Is(err, state.ErrNotFound) {
		s.id = newID()
		err = db.Put("meta", "server", s.id)
	}
	if err != nil {
		return nil, err
	}
	jobs, err := state.List[Job](db, "job")
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		if !spec.ValidName(j.ID) {
			return nil, errors.New("invalid saved job ID")
		}
		if !j.State.Terminal() {
			if n := len(j.Attempts); n > 0 && j.live(n-1) && !seen[j.current().Worker] {
				return nil, errors.New("configuration removes a worker with unfinished attempts")
			}
			for _, step := range j.Steps {
				if err := s.checkConfig(step.Spec.Task); err != nil {
					return nil, fmt.Errorf("unfinished job %s: %w", j.Out, err)
				}
			}
		}
		s.installLocked(j)
	}
	s.images = image.NewResolver(filepath.Join(root, "blobs"), root, c.Tools, imageCache{db})
	return s, nil
}

// stateDirs caches each resolved home, so a restart while that worker is down still finds its state.
func stateDirs(db *state.Store, workers []worker.Config, home func(context.Context, string) (string, error)) ([]worker.Config, error) {
	workers = slices.Clone(workers)
	for i, w := range workers {
		if w.StateDir != "" {
			continue
		}
		var dir string
		err := db.Get("home", w.Host, &dir)
		if errors.Is(err, state.ErrNotFound) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			dir, err = home(ctx, w.Host)
			cancel()
			if err == nil && !path.IsAbs(dir) {
				err = fmt.Errorf("home %q is not an absolute path", dir)
			}
			if err == nil {
				err = db.Put("home", w.Host, dir)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("worker %s: find its home for the default state_dir, or set state_dir: %w", w.Host, err)
		}
		workers[i].StateDir = path.Join(dir, worker.DefaultStateDir)
	}
	return workers, nil
}

func (s *Server) Close() error {
	s.images.Close()
	return s.store.Close()
}
func Socket(root string) string { return filepath.Join(root, "server.sock") }

type imageCache struct{ store *state.Store }

func (c imageCache) Load(key string) (image.Entry, bool, error) {
	var e image.Entry
	err := c.store.Get("image", key, &e)
	if errors.Is(err, state.ErrNotFound) {
		return e, false, nil
	}
	return e, err == nil, err
}
func (c imageCache) Save(key string, e image.Entry) error { return c.store.Put("image", key, e) }
func (j *Job) current() *Attempt                          { return &j.Attempts[len(j.Attempts)-1] }
func newID() string                                       { return strings.ToLower(rand.Text()) }

// live reports whether attempt i may still run. A requeued job's last attempt is over: dispatch
// replaces it in the same write that leaves the queued state. A delivering job's has been collected.
func (j Job) live(i int) bool {
	return i == len(j.Attempts)-1 && !j.State.Terminal() && j.State != Queued && j.State != Delivering
}

func (j Job) clone() Job {
	j.Attempts = slices.Clone(j.Attempts)
	return j
}
func (s *Server) job(id string) (Job, bool, error) {
	s.mu.Lock()
	j, unfinished := s.jobs[id]
	_, known := s.records[id]
	s.mu.Unlock()
	if unfinished {
		return j.clone(), true, nil
	}
	if !known {
		return Job{}, false, nil
	}
	// A commit writes the store before it installs, so the store is never behind memory.
	err := s.store.Get("job", id, &j)
	return j, err == nil, err
}

// within reports whether file is dir or lies under it; both are clean absolute paths.
func within(file, dir string) bool {
	return file == dir || strings.HasPrefix(file, strings.TrimSuffix(dir, "/")+"/")
}

// Filter keeps the jobs that match every field it sets, and any value within one.
type Filter struct {
	IDs    []string
	States []State
	Outs   []string
	// Under keeps the jobs whose Out is under any of these directories.
	Under []string
}

func (f Filter) keeps(r Summary) bool {
	return (len(f.States) == 0 || slices.Contains(f.States, r.State)) && (len(f.Outs) == 0 || slices.Contains(f.Outs, r.Out)) &&
		(len(f.Under) == 0 || slices.ContainsFunc(f.Under, func(dir string) bool { return within(r.Out, dir) }))
}

func (s *Server) List(f Filter) []Summary {
	s.mu.Lock()
	var jobs []Summary
	add := func(r record) {
		if f.keeps(r.Summary) {
			r.Pending = maps.Clone(s.pending[r.ID])
			jobs = append(jobs, r.Summary)
		}
	}
	if f.IDs != nil {
		for _, id := range f.IDs {
			if r, ok := s.records[id]; ok {
				add(r)
			}
		}
	} else {
		for _, r := range s.records {
			add(r)
		}
	}
	s.mu.Unlock()
	slices.SortFunc(jobs, func(a, b Summary) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return jobs
}

func (s *Server) unfinished() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := slices.Collect(maps.Keys(s.jobs))
	slices.SortFunc(ids, func(a, b string) int { return s.jobs[a].CreatedAt.Compare(s.jobs[b].CreatedAt) })
	return ids
}

// save drops a write to a job that has finished meanwhile: no job leaves a terminal state, so the
// writer worked from a copy taken before it finished.
func (s *Server) save(j Job) error {
	s.write.Lock()
	defer s.write.Unlock()
	if _, unfinished := s.jobs[j.ID]; !unfinished {
		return nil
	}
	s.refreshLocked(&j)
	return s.commit(j)
}

// refresh adopts cancel and stop requests that arrived after reconcile copied j.
func (s *Server) refresh(j *Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked(j)
}

func (s *Server) refreshLocked(j *Job) {
	old, ok := s.jobs[j.ID]
	if !ok {
		return
	}
	j.CancelRequested = j.CancelRequested || old.CancelRequested
	// A stop ends one step, so it does not follow the job to the next.
	if old.Step == j.Step || j.CancelRequested {
		j.StopRequested = j.StopRequested || old.StopRequested
		j.StopReason = cmp.Or(old.StopReason, j.StopReason)
	}
	if j.CancelRequested && j.State.Terminal() {
		j.State = Cancelled
	}
}

func (j Job) stopKind() StopKind {
	switch {
	case j.CancelRequested:
		return Cancel
	case j.StopRequested:
		return Stop
	}
	return ""
}

// commit writes jobs in one transaction, then installs them; the caller holds s.write. A failed write
// latches s.fault so no later mutation diverges from the store.
func (s *Server) commit(jobs ...Job) error {
	if s.fault != nil || len(jobs) == 0 {
		return s.fault
	}
	for i := range jobs {
		if jobs[i].State.Terminal() && jobs[i].FinishedAt.IsZero() {
			jobs[i].FinishedAt = time.Now().UTC()
		}
	}
	err := s.store.Update(func(tx *state.Tx) error {
		for _, j := range jobs {
			if err := tx.Put("job", j.ID, j); err != nil {
				return err
			}
		}
		return nil
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.fault = err
		return err
	}
	for _, j := range jobs {
		s.installLocked(j)
	}
	return nil
}

func (s *Server) installLocked(j Job) {
	s.records[j.ID] = record{Summary: j.summary(), key: j.Key}
	if j.State != Queued {
		delete(s.pending, j.ID)
	}
	for _, at := range j.Attempts {
		s.attempts[at.ID] = j.ID
	}
	if j.State.Terminal() {
		delete(s.jobs, j.ID)
		if s.outs[j.Out] == j.ID {
			delete(s.outs, j.Out)
		}
	} else {
		s.jobs[j.ID] = j.clone()
		s.outs[j.Out] = j.ID
	}
}

type staged[V any] struct {
	value V
	err   error
}

type staging struct {
	images map[image.Source]staged[image.Resolved]
	inputs map[string]staged[string]
}

// Snapshots read the submitter's files, so only a few share the disk at once.
const snapshots = 4

// stage takes only requests that passed check.
func (s *Server) stage(ctx context.Context, requests ...Request) *staging {
	var images []image.Source
	var inputs []string
	for _, r := range requests {
		for _, step := range r.steps() {
			images = append(images, step.Image)
		}
		for _, source := range r.Inputs {
			inputs = append(inputs, source)
		}
	}
	st := &staging{}
	var wg sync.WaitGroup
	wg.Go(func() {
		st.images = each(images, len(images), func(src image.Source) (image.Resolved, error) { return s.images.Resolve(ctx, src) })
	})
	wg.Go(func() {
		st.inputs = each(inputs, snapshots, func(source string) (string, error) { return content.PutTree(s.images.Blobs, source) })
	})
	wg.Wait()
	return st
}

// each calls do once per distinct key, at most limit at a time.
func each[K comparable, V any](keys []K, limit int, do func(K) (V, error)) map[K]staged[V] {
	out := map[K]staged[V]{}
	var distinct []K
	for _, k := range keys {
		if _, seen := out[k]; !seen {
			out[k] = staged[V]{}
			distinct = append(distinct, k)
		}
	}
	results := make([]staged[V], len(distinct))
	slots := make(chan struct{}, max(limit, 1))
	var wg sync.WaitGroup
	for i, k := range distinct {
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i].value, results[i].err = do(k)
		})
	}
	wg.Wait()
	for i, k := range distinct {
		out[k] = results[i]
	}
	return out
}

func absolute(file string) bool {
	return filepath.IsAbs(file) && filepath.Clean(file) == file && file != "/"
}

func (r Request) Validate() error {
	if !absolute(r.Out) {
		return errors.New("out must be a clean absolute path other than /")
	}
	if len(r.Steps) > 0 && !reflect.ValueOf(r.Step).IsZero() {
		return errors.New("give either steps or one step's fields at the top level")
	}
	if len(r.Steps) > MaxSteps {
		return fmt.Errorf("a job has at most %d steps", MaxSteps)
	}
	var names []string
	for _, step := range r.steps() {
		if err := step.Image.Validate(); err != nil {
			return err
		}
		if err := step.Validate(); err != nil {
			return err
		}
		if step.Name != "" {
			names = append(names, step.Name)
		}
	}
	if !spec.ValidNameSet(names) {
		return errors.New("step names must be unique and valid")
	}
	if err := r.Retry.validate(); err != nil {
		return err
	}
	for name, source := range r.Inputs {
		if !spec.ValidName(name) {
			return errors.New("invalid input name")
		}
		if !absolute(source) {
			return fmt.Errorf("input %s must be a clean absolute path other than /", name)
		}
		if within(source, r.Out) {
			return fmt.Errorf("input %s lies in the job's own out", name)
		}
	}
	return nil
}

func (s *Server) check(r Request) error {
	if err := r.Validate(); err != nil {
		return err
	}
	for _, step := range r.steps() {
		if err := s.checkConfig(step.Task); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) secret(name string) (string, error) {
	if s.secretsDir == "" {
		return "", fmt.Errorf("secret %s needs a secrets directory", name)
	}
	data, err := os.ReadFile(filepath.Join(s.secretsDir, name))
	if err != nil {
		return "", fmt.Errorf("secret %s: %w", name, err)
	}
	return string(data), spec.CheckSecret(name, string(data))
}

func (s *Server) checkConfig(t spec.Task) error {
	for _, name := range t.Services {
		if _, ok := s.services[name]; !ok {
			return fmt.Errorf("service %q is unknown; add it to \"services\" in the server config", name)
		}
	}
	for _, name := range t.Secrets {
		if _, err := s.secret(name); err != nil {
			return err
		}
	}
	tagged := slices.DeleteFunc(slices.Clone(s.workers), func(w worker.Config) bool { return !w.Carries(t.Tags) })
	if len(tagged) == 0 {
		return fmt.Errorf("no configured worker has all tags %q; add them to a worker's \"tags\" in the server config", t.Tags)
	}
	if r := t.Resources; !slices.ContainsFunc(tagged, func(w worker.Config) bool { return w.Fits(r) }) {
		return fmt.Errorf("no matching worker is configured with %d cores, %d MiB and %d GPUs", r.Cores, r.MemoryMiB, r.GPUs)
	}
	return nil
}

// delivered reports whether out already holds results that no unfinished job is writing.
func (s *Server) delivered(out string) (bool, error) {
	s.mu.Lock()
	_, unfinished := s.outs[out]
	s.mu.Unlock()
	if unfinished {
		return false, nil
	}
	f, err := os.Open(out)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return len(names) > 0, err
}

// waiting finds the inputs that lie in an unfinished job's Out or in one of outs, the earlier lines of
// a batch: they are snapshotted once that job delivers, not now.
func (s *Server) waiting(r Request, outs []string) map[string]string {
	if len(r.Inputs) == 0 {
		return nil
	}
	s.mu.Lock()
	outs = slices.Concat(outs, slices.Collect(maps.Keys(s.outs)))
	s.mu.Unlock()
	var waiting map[string]string
	for name, source := range r.Inputs {
		if slices.ContainsFunc(outs, func(out string) bool { return within(source, out) }) {
			if waiting == nil {
				waiting = map[string]string{}
			}
			waiting[name] = source
		}
	}
	return waiting
}

// prepare takes a request that passed check and was staged without the inputs that wait for other
// jobs.
func (s *Server) prepare(r Request, waiting map[string]string, st *staging) (Job, error) {
	var inputs map[string]string
	if len(r.Inputs) > len(waiting) {
		inputs = map[string]string{}
		for name, source := range r.Inputs {
			if _, ok := waiting[name]; ok {
				continue
			}
			input := st.inputs[source]
			if input.err != nil {
				return Job{}, input.err
			}
			inputs[name] = input.value
		}
	}
	j := Job{ID: newID(), Out: r.Out, State: Queued, CreatedAt: time.Now().UTC(), Retry: r.Retry, Inputs: maps.Clone(r.Inputs), Waiting: slices.Sorted(maps.Keys(waiting))}
	for _, step := range r.steps() {
		img := st.images[step.Image]
		if img.err != nil {
			return Job{}, img.err
		}
		sp := spec.Spec{Image: img.value.ID, Inputs: inputs, Task: step.Task}.Normalize()
		for _, w := range s.workers {
			if w.Carries(sp.Tags) {
				if err := w.CheckLaunch(sp, slices.Collect(maps.Keys(waiting))); err != nil {
					return Job{}, err
				}
			}
		}
		j.Steps = append(j.Steps, Stage{Name: step.Name, Spec: sp, Archive: img.value.Blob, ImageOrigin: img.value.Origin})
	}
	return j, nil
}

// admit keys jobs by Out: an unfinished job there with the same request is returned, and a different
// one, or one whose Out nests with this one's, refuses it.
func (s *Server) admit(jobs []Job, results []*BatchResult) {
	s.write.Lock()
	defer s.write.Unlock()
	pending := map[string]record{}
	fresh := []Job{}
	for i, j := range jobs {
		result := results[i]
		j.Key = jobKey(j)
		old, ok := pending[j.Out]
		if id, unfinished := s.outs[j.Out]; unfinished {
			old, ok = s.records[id], true
		}
		switch {
		case ok && old.key == j.Key:
			result.ID, result.State, result.Existing = old.ID, old.State, true
		case ok:
			result.Error = "out already has an unfinished job with a different request"
		default:
			if other := s.nested(j.Out, pending); other != "" {
				result.Error = fmt.Sprintf("out nests with %s, which an unfinished job writes", other)
				continue
			}
			var err error
			if j.After, err = s.upstream(j, pending); err != nil {
				result.Error = err.Error()
				continue
			}
			pending[j.Out] = record{Summary: j.summary(), key: j.Key}
			fresh = append(fresh, j)
			result.ID, result.State = j.ID, j.State
		}
	}
	if err := s.commit(fresh...); err != nil {
		for _, result := range results {
			if result.Error == "" {
				*result = BatchResult{Line: result.Line, Out: result.Out, Error: err.Error()}
			}
		}
	}
}

// The caller holds s.write.
func (s *Server) claimed(pending map[string]record) iter.Seq2[string, string] {
	return func(yield func(string, string) bool) {
		for out, id := range s.outs {
			if !yield(out, id) {
				return
			}
		}
		for out, r := range pending {
			if !yield(out, r.ID) {
				return
			}
		}
	}
}

// upstream finds the unfinished jobs whose Out holds each of j's waiting inputs; the caller holds
// s.write.
func (s *Server) upstream(j Job, pending map[string]record) ([]string, error) {
	var ids []string
	for _, name := range j.Waiting {
		source := j.Inputs[name]
		id := ""
		for out, other := range s.claimed(pending) {
			if within(source, out) {
				id = other
				break
			}
		}
		if id == "" {
			return nil, fmt.Errorf("input %s lies in an out that no unfinished job writes any more; submit again", name)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// nested finds an unfinished job's Out that contains out or lies inside it; the caller holds s.write.
func (s *Server) nested(out string, pending map[string]record) string {
	for other := range s.claimed(pending) {
		if within(out, other) || within(other, out) {
			return other
		}
	}
	return ""
}

func (s *Server) Run(ctx context.Context, port uint16) error {
	web, err := listenWeb(port)
	if err != nil {
		return err
	}
	defer func() { _ = web.Close() }()
	fmt.Fprintf(os.Stderr, "Web UI: http://%s\n", web.Addr().String())
	listener, err := ListenUnix(Socket(s.root))
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, remote := range s.workers {
		wg.Go(func() { s.work(child, remote) })
	}
	wg.Go(func() { s.tidy(child) })
	api := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	public := &http.Server{Handler: s.WebHandler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
	done := make(chan error, 2)
	go func() { done <- api.Serve(listener) }()
	go func() { done <- public.Serve(web) }()
	select {
	case <-ctx.Done():
	case err = <-done:
	}
	cancel()
	shutdown, end := context.WithTimeout(context.Background(), 5*time.Second)
	defer end()
	err = errors.Join(err, api.Shutdown(shutdown), public.Shutdown(shutdown))
	_ = api.Close()
	_ = public.Close()
	wg.Wait()
	return err
}

func (s *Server) Handler() http.Handler {
	mux := s.routes()
	mux.HandleFunc("POST /v1/batches", s.serveBatch)
	return mux
}

// routes are also served to the browser, so none submits: a submission can make the server build nix
// images, read its files, and hand jobs every configured secret.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/workers", func(w http.ResponseWriter, r *http.Request) { WriteJSON(w, http.StatusOK, s.Workers()) })
	mux.HandleFunc("GET /v1/jobs/{id}/logs/{stream}", s.serveLogs)
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", stopHandler(s.Cancel))
	mux.HandleFunc("POST /v1/jobs/{id}/stop", stopHandler(s.Stop))
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		f := Filter{IDs: query["id"], Outs: query["out"], Under: query["under"]}
		for _, state := range query["state"] {
			f.States = append(f.States, State(state))
		}
		WriteJSON(w, http.StatusOK, s.List(f))
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := s.lookup(w, r)
		if !ok {
			return
		}
		s.mu.Lock()
		pending := maps.Clone(s.pending[j.ID])
		s.mu.Unlock()
		WriteJSON(w, http.StatusOK, struct {
			Job
			Pending map[string]string `json:"pending,omitempty"`
		}{j, pending})
	})
	return mux
}

// lookup answers 404 or 500 itself when it finds no job.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request) (Job, bool) {
	j, ok, err := s.job(r.PathValue("id"))
	switch {
	case err != nil:
		http.Error(w, err.Error(), 500)
	case !ok:
		http.NotFound(w, r)
	}
	return j, ok && err == nil
}
