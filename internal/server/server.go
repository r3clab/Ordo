package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
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

type Request struct {
	ID     string            `json:"id,omitempty"`
	Image  image.Source      `json:"image"`
	Inputs map[string]string `json:"inputs,omitempty"`
	spec.Task
	Retry RetryPolicy `json:"retry,omitempty"`
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
	Succeeded   State = "succeeded"
	Failed      State = "failed"
	Cancelled   State = "cancelled"
	Skipped     State = "skipped"
)

func (s State) Terminal() bool {
	return s == Succeeded || s == Failed || s == Cancelled || s == Skipped
}

type Job struct {
	ID              string            `json:"id"`
	Spec            spec.Spec         `json:"spec"`
	Archive         string            `json:"archive"`
	State           State             `json:"state"`
	Error           string            `json:"error,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	Attempts        []Attempt         `json:"attempts,omitempty"`
	Key             string            `json:"submission_key,omitempty"`
	Workflow        string            `json:"workflow,omitempty"`
	Depends         []string          `json:"depends_on,omitempty"`
	From            map[string]string `json:"from,omitempty"`
	Retry           RetryPolicy       `json:"retry,omitempty"`
	CancelRequested bool              `json:"cancel_requested,omitempty"`
	StopRequested   bool              `json:"stop_requested,omitempty"`
	StopReason      string            `json:"stop_reason,omitempty"`
	ImageOrigin     *image.Origin     `json:"image_origin,omitempty"`
}

// Summary is how GET /v1/jobs lists a job; GET /v1/jobs/{id} returns all of it.
type Summary struct {
	ID              string         `json:"id"`
	State           State          `json:"state"`
	Workflow        string         `json:"workflow,omitempty"`
	Image           string         `json:"image"`
	Resources       spec.Resources `json:"resources"`
	Worker          string         `json:"worker,omitempty"`
	Attempts        int            `json:"attempts,omitempty"`
	CreatedAt       time.Time      `json:"created_at"`
	Error           string         `json:"error,omitempty"`
	Depends         []string       `json:"depends_on,omitempty"`
	CancelRequested bool           `json:"cancel_requested,omitempty"`
}

func (j Job) summary() Summary {
	s := Summary{ID: j.ID, State: j.State, Workflow: j.Workflow, Image: j.Spec.Image, Resources: j.Spec.Resources, Attempts: len(j.Attempts), CreatedAt: j.CreatedAt, Error: j.Error, Depends: j.Depends, CancelRequested: j.CancelRequested}
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
	write    sync.Mutex
	mu       sync.Mutex
	Root     string
	store    *state.Store
	id       string
	workers  []worker.Config
	services map[string]string
	secrets  map[string]string
	// jobs holds only unfinished jobs; a finished one is read back from the store.
	jobs     map[string]Job
	records  map[string]record
	attempts map[string]string
	busy     map[string]bool
	fault    error
	nodes    map[string]WorkerStatus
	images   *image.Resolver
}

type Config struct {
	Root     string
	Workers  []worker.Config
	Services map[string]string
	Secrets  map[string]string
	Tools    image.Tools
	// Home finds a worker's home for its default state directory; nil uses ssh.Home.
	Home func(ctx context.Context, host string) (string, error)
}

func Open(c Config) (_ *Server, err error) {
	if len(c.Workers) == 0 || len(c.Workers) > 10 {
		return nil, errors.New("configure 1-10 workers")
	}
	for name, value := range c.Secrets {
		if err := spec.CheckSecret(name, value); err != nil {
			return nil, err
		}
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
		home = ssh.Home
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
	s := &Server{Root: root, store: db, workers: workers, services: maps.Clone(c.Services), secrets: maps.Clone(c.Secrets), jobs: map[string]Job{}, records: map[string]record{}, attempts: map[string]string{}, busy: map[string]bool{}, nodes: map[string]WorkerStatus{}}
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
			if err := s.checkConfig(j.Spec); err != nil {
				return nil, fmt.Errorf("unfinished job %s: %w", j.ID, err)
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
// replaces it in the same write that leaves the queued state.
func (j Job) live(i int) bool {
	return i == len(j.Attempts)-1 && !j.State.Terminal() && j.State != Queued
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

// Filter keeps the jobs that match every field it sets, and any value within one.
type Filter struct {
	IDs       []string
	States    []State
	Workflows []string
}

func (f Filter) keeps(r Summary) bool {
	return (len(f.States) == 0 || slices.Contains(f.States, r.State)) && (len(f.Workflows) == 0 || slices.Contains(f.Workflows, r.Workflow))
}

func (s *Server) List(f Filter) []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobs []Summary
	add := func(r record) {
		if f.keeps(r.Summary) {
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
	j.StopRequested = j.StopRequested || old.StopRequested
	j.StopReason = cmp.Or(old.StopReason, j.StopReason)
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
	for _, at := range j.Attempts {
		s.attempts[at.ID] = j.ID
	}
	if j.State.Terminal() {
		delete(s.jobs, j.ID)
	} else {
		s.jobs[j.ID] = j.clone()
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
		images = append(images, r.Image)
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

func (r Request) Validate() error {
	if err := r.Image.Validate(); err != nil {
		return err
	}
	if err := r.Retry.validate(); err != nil {
		return err
	}
	if r.ID != "" && !spec.ValidName(r.ID) {
		return errors.New("invalid job ID")
	}
	for name := range r.Inputs {
		if !spec.ValidName(name) {
			return errors.New("invalid input name")
		}
	}
	return r.Task.Validate()
}

func (s *Server) check(r Request) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return s.checkConfig(r.spec())
}

func (s *Server) checkConfig(sp spec.Spec) error {
	for _, name := range sp.Services {
		if _, ok := s.services[name]; !ok {
			return fmt.Errorf("service %q is unknown; add it to \"services\" in the server config", name)
		}
	}
	for _, name := range sp.Secrets {
		if _, ok := s.secrets[name]; !ok {
			return fmt.Errorf("secret %q is unknown; add it to \"secrets\" in the server config", name)
		}
	}
	if !slices.ContainsFunc(s.workers, func(w worker.Config) bool { return w.Carries(sp.Tags) }) {
		return fmt.Errorf("no configured worker has all tags %q; add them to a worker's \"tags\" in the server config", sp.Tags)
	}
	return nil
}

func (r Request) spec() spec.Spec {
	return spec.Spec{Task: r.Task}.Normalize()
}

// prepare takes a request that passed check and was staged.
func (s *Server) prepare(r Request, from map[string]string, st *staging) (Job, error) {
	if r.ID == "" {
		r.ID = newID()
	}
	img := st.images[r.Image]
	if img.err != nil {
		return Job{}, img.err
	}
	j := Job{ID: r.ID, Spec: r.spec(), Archive: img.value.Blob, ImageOrigin: img.value.Origin, State: Queued, CreatedAt: time.Now().UTC(), From: from, Retry: r.Retry}
	j.Spec.Image = img.value.ID
	if len(r.Inputs) > 0 {
		j.Spec.Inputs = map[string]string{}
		for name, source := range r.Inputs {
			input := st.inputs[source]
			if input.err != nil {
				return Job{}, input.err
			}
			j.Spec.Inputs[name] = input.value
		}
	}
	imported := slices.Collect(maps.Keys(from))
	for _, w := range s.workers {
		if w.Carries(j.Spec.Tags) {
			if err := w.CheckLaunch(j.Spec, imported); err != nil {
				return Job{}, err
			}
		}
	}
	return j, nil
}
func (s *Server) Submit(ctx context.Context, r Request) (Job, error) {
	if err := s.check(r); err != nil {
		return Job{}, err
	}
	j, err := s.prepare(r, nil, s.stage(ctx, r))
	if err != nil {
		return Job{}, err
	}
	if err := s.admit([]Job{j})[0].err; err != nil {
		return Job{}, err
	}
	j, _, err = s.job(j.ID)
	return j, err
}

type admission struct {
	state    State
	existing bool
	err      error
}

func (s *Server) admit(jobs []Job) []admission {
	out := make([]admission, len(jobs))
	s.write.Lock()
	defer s.write.Unlock()
	pending := map[string]record{}
	fresh := []Job{}
	for i, j := range jobs {
		j.Key = jobKey(j)
		old, ok := s.records[j.ID]
		if !ok {
			old, ok = pending[j.ID]
		}
		switch {
		case !ok:
			pending[j.ID] = record{Summary: j.summary(), key: j.Key}
			fresh = append(fresh, j)
			out[i].state = j.State
		case old.key != j.Key:
			out[i].err = errors.New("job ID already has a different immutable specification")
		default:
			out[i] = admission{state: old.State, existing: true}
		}
	}
	if err := s.commit(fresh...); err != nil {
		for i := range out {
			out[i].err = cmp.Or(out[i].err, err)
		}
	}
	return out
}

func (s *Server) Run(ctx context.Context, address string) error {
	web, err := listenWeb(address)
	if err != nil {
		return err
	}
	defer func() { _ = web.Close() }()
	token, err := s.Token()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Web UI: http://%s (token file: %s)\n", web.Addr().String(), filepath.Join(s.Root, "token"))
	listener, err := ListenUnix(Socket(s.Root))
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
	api := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	public := &http.Server{Handler: s.WebHandler(token), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second}
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
	mux.HandleFunc("GET /v1/jobs/{id}/result", s.serveResult)
	return mux
}

// routes are also served to the browser, so none submits: a submission can make the server build nix
// images, read its files, and hand jobs every configured secret.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/workers", func(w http.ResponseWriter, r *http.Request) { WriteJSON(w, http.StatusOK, s.Workers()) })
	mux.HandleFunc("GET /v1/jobs/{id}/logs/{stream}", s.serveLogs)
	cancel := func(workflow bool) http.HandlerFunc {
		return StopHandler(func(id, reason string) error { return s.Cancel(id, workflow, reason) })
	}
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", cancel(false))
	mux.HandleFunc("POST /v1/workflows/{id}/cancel", cancel(true))
	mux.HandleFunc("POST /v1/jobs/{id}/stop", StopHandler(s.Stop))
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		f := Filter{IDs: query["id"], Workflows: query["workflow"]}
		for _, state := range query["state"] {
			f.States = append(f.States, State(state))
		}
		WriteJSON(w, http.StatusOK, s.List(f))
	})
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := s.lookup(w, r)
		if ok {
			WriteJSON(w, http.StatusOK, j)
		}
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
