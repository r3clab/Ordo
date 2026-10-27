package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/state"
	"github.com/r3clab/Ordo/internal/worker"
)

type Request struct {
	ID        string            `json:"id,omitempty"`
	Image     image.Source      `json:"image"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Resources spec.Resources    `json:"resources"`
	spec.Sandbox
	Secrets        map[string]string `json:"secrets,omitempty"`
	Services       []string          `json:"services,omitempty"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitempty"`
	Retry          RetryPolicy       `json:"retry,omitempty"`
	Tags           []string          `json:"tags,omitempty"`
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
	Parameters      map[string]string `json:"parameters,omitempty"`
	Retry           RetryPolicy       `json:"retry,omitempty"`
	CancelRequested bool              `json:"cancel_requested,omitempty"`
	StopRequested   bool              `json:"stop_requested,omitempty"`
	StopReason      string            `json:"stop_reason,omitempty"`
	ImageOrigin     *image.Origin     `json:"image_origin,omitempty"`
}

type Server struct {
	mu       sync.Mutex
	Root     string
	store    *state.Store
	id       string
	workers  []worker.Config
	services map[string]string
	jobs     map[string]Job
	busy     map[string]bool
	fault    error
	nodes    map[string]WorkerStatus
	images   *image.Resolver
}

// Caller is who submits: only local socket callers may make the server run nix builds.
type Caller int

const (
	Web Caller = iota
	Local
)

var ErrNixUntrusted = errors.New("nix images require the local API socket")

func Open(root string, workers []worker.Config, services map[string]string, tools image.Tools) (_ *Server, err error) {
	if len(workers) == 0 || len(workers) > 10 {
		return nil, errors.New("configure 1-10 workers")
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
		for name, address := range services {
			if _, err := w.ServiceForward(name, address); err != nil {
				return nil, err
			}
		}
	}
	root, err = filepath.Abs(root)
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
	s := &Server{Root: root, store: db, workers: workers, services: maps.Clone(services), jobs: map[string]Job{}, busy: map[string]bool{}, nodes: map[string]WorkerStatus{}}
	// Containers carry this ID, so a worker account shared with another server is detected.
	err = db.Get("meta", "server", &s.id)
	if errors.Is(err, sql.ErrNoRows) {
		s.id = strings.ToLower(rand.Text())
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
			for _, name := range j.Spec.Services {
				if _, ok := services[name]; !ok {
					return nil, fmt.Errorf("configuration removes service %s, which unfinished job %s uses", name, j.ID)
				}
			}
			if !slices.ContainsFunc(workers, func(w worker.Config) bool { return w.Carries(j.Spec.Tags) }) {
				return nil, fmt.Errorf("configuration leaves no worker with all tags %q, which unfinished job %s uses", j.Spec.Tags, j.ID)
			}
		}
		s.jobs[j.ID] = j
	}
	s.images = image.NewResolver(filepath.Join(root, "blobs"), root, tools, imageCache{db})
	return s, nil
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
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	return e, err == nil, err
}
func (c imageCache) Save(key string, e image.Entry) error { return c.store.Put("image", key, e) }
func (j *Job) current() *Attempt                          { return &j.Attempts[len(j.Attempts)-1] }

// live reports whether attempt i may still run. A requeued job's last attempt is over: dispatch
// replaces it in the same write that leaves the queued state.
func (j Job) live(i int) bool {
	return i == len(j.Attempts)-1 && !j.State.Terminal() && j.State != Queued
}

func (j Job) clone() Job {
	j.Attempts = slices.Clone(j.Attempts)
	return j
}
func (s *Server) job(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	return j.clone(), ok
}
func (s *Server) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		jobs = append(jobs, j.clone())
	}
	slices.SortFunc(jobs, func(a, b Job) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return jobs
}

func (s *Server) unfinished() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, j := range s.jobs {
		if !j.State.Terminal() {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b string) int { return s.jobs[a].CreatedAt.Compare(s.jobs[b].CreatedAt) })
	return ids
}
func (s *Server) save(j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked(&j)
	return s.commitLocked(nil, j)
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

// commitLocked writes new jobs' secrets with the jobs and drops a job's secrets in the write that
// finishes it, so secrets exist exactly while their job is unfinished. A failed write latches s.fault
// so no later mutation diverges from the store.
func (s *Server) commitLocked(secrets map[string]map[string]string, jobs ...Job) error {
	if s.fault != nil || len(jobs) == 0 {
		return s.fault
	}
	err := s.store.Update(func(tx *state.Tx) error {
		for id, values := range secrets {
			if err := tx.Put("secret", id, values); err != nil {
				return err
			}
		}
		for _, j := range jobs {
			if err := tx.Put("job", j.ID, j); err != nil {
				return err
			}
			if j.State.Terminal() && len(j.Spec.Secrets) > 0 {
				if err := tx.Delete("secret", j.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		s.fault = err
		return err
	}
	for _, j := range jobs {
		s.jobs[j.ID] = j.clone()
	}
	return nil
}

type stagedImage struct {
	image.Resolved
	err error
}

type staging struct {
	resolver *image.Resolver
	mu       sync.Mutex
	images   map[image.Source]stagedImage
	inputs   map[string]string
}

func (s *Server) stage(ctx context.Context, requests ...Request) *staging {
	st := &staging{resolver: s.images, images: map[image.Source]stagedImage{}, inputs: map[string]string{}}
	seen := map[image.Source]bool{}
	var wg sync.WaitGroup
	for _, r := range requests {
		if r.Image.IsNix() && !seen[r.Image] && r.Check(s.services, s.workers) == nil {
			seen[r.Image] = true
			wg.Go(func() { _, _ = st.image(ctx, r.Image) })
		}
	}
	wg.Wait()
	return st
}

func (st *staging) image(ctx context.Context, src image.Source) (image.Resolved, error) {
	st.mu.Lock()
	staged, ok := st.images[src]
	st.mu.Unlock()
	if !ok {
		staged.Resolved, staged.err = st.resolver.Resolve(ctx, src)
		st.mu.Lock()
		st.images[src] = staged
		st.mu.Unlock()
	}
	return staged.Resolved, staged.err
}

func (r Request) Check(services map[string]string, workers []worker.Config) error {
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
	if err := r.spec().Validate(r.Secrets); err != nil {
		return err
	}
	for _, name := range r.Services {
		if _, ok := services[name]; !ok {
			return fmt.Errorf("service %q is unknown; configure it with ordo server --service", name)
		}
	}
	if len(r.Tags) > 0 && !slices.ContainsFunc(workers, func(w worker.Config) bool { return w.Carries(r.Tags) }) {
		return fmt.Errorf("no configured worker has all tags %q; add them to a worker's \"tags\" in the ordo server --workers file", r.Tags)
	}
	return nil
}

func (r Request) nix() bool { return r.Image.IsNix() }

func (r Request) spec() spec.Spec {
	return spec.Spec{Args: r.Args, Resources: r.Resources, Sandbox: r.Sandbox, Secrets: slices.Collect(maps.Keys(r.Secrets)), Services: r.Services, TimeoutSeconds: r.TimeoutSeconds, Tags: r.Tags}.Normalize()
}

// prepare also takes the workflow inputs a job will import, so the launch size check counts them.
func (s *Server) prepare(ctx context.Context, r Request, from map[string]string, st *staging) (Job, error) {
	if err := r.Check(s.services, s.workers); err != nil {
		return Job{}, err
	}
	if r.ID == "" {
		r.ID = strings.ToLower(rand.Text())
	}
	img, err := st.image(ctx, r.Image)
	if err != nil {
		return Job{}, err
	}
	j := Job{ID: r.ID, Spec: r.spec(), Archive: img.Blob, ImageOrigin: img.Origin, State: Queued, CreatedAt: time.Now().UTC(), From: from, Retry: r.Retry}
	j.Spec.Image = img.ID
	if len(r.Inputs) > 0 {
		j.Spec.Inputs = map[string]string{}
		for name, source := range r.Inputs {
			hash, ok := st.inputs[source]
			if !ok {
				if hash, err = content.PutTree(s.images.Blobs, source); err != nil {
					return Job{}, err
				}
				st.inputs[source] = hash
			}
			j.Spec.Inputs[name] = hash
		}
	}
	worst := j.Spec
	worst.Inputs = maps.Clone(worst.Inputs)
	for name := range from {
		if worst.Inputs == nil {
			worst.Inputs = map[string]string{}
		}
		// Imported outputs are known only at dispatch, by their SHA-256 hex digest.
		worst.Inputs[name] = strings.Repeat("0", 64)
	}
	for _, w := range s.workers {
		if w.Carries(j.Spec.Tags) {
			if err := w.CheckLaunch(worst, r.Secrets); err != nil {
				return Job{}, err
			}
		}
	}
	return j, nil
}
func (s *Server) Submit(ctx context.Context, r Request) (Job, error) {
	j, err := s.prepare(ctx, r, nil, s.stage(ctx, r))
	if err != nil {
		return Job{}, err
	}
	admitted := s.admit([]Job{j}, []map[string]string{r.Secrets})[0]
	return admitted.job, admitted.err
}

type admission struct {
	job      Job
	existing bool
	err      error
}

func (s *Server) admit(jobs []Job, secrets []map[string]string) []admission {
	out := make([]admission, len(jobs))
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := map[string]Job{}
	fresh := []Job{}
	values := map[string]map[string]string{}
	for i, j := range jobs {
		j.Key = jobKey(j)
		old, ok := s.jobs[j.ID]
		if !ok {
			old, ok = pending[j.ID]
		}
		switch {
		case !ok:
			pending[j.ID] = j
			fresh = append(fresh, j)
			if len(secrets[i]) > 0 {
				values[j.ID] = secrets[i]
			}
			out[i].job = j
		case cmp.Or(old.Key, jobKey(old)) != j.Key:
			out[i].err = errors.New("job ID already has a different immutable specification")
		default:
			out[i] = admission{job: old, existing: true}
		}
	}
	if err := cmp.Or(s.fault, s.commitLocked(values, fresh...)); err != nil {
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

func (s *Server) Handler() http.Handler { return s.handler(Local) }

func submitError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, ErrNixUntrusted) {
		status = http.StatusForbidden
	}
	http.Error(w, err.Error(), status)
}

func submitRoute[T interface{ nix() bool }, R any](caller Caller, limit int64, submit func(context.Context, T) (R, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req T
		if err := DecodeJSON(http.MaxBytesReader(w, r.Body, limit), &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if caller != Local && req.nix() {
			submitError(w, ErrNixUntrusted)
			return
		}
		result, err := submit(r.Context(), req)
		if err != nil {
			submitError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, result)
	}
}

func (s *Server) handler(caller Caller) http.Handler {
	mux := http.NewServeMux()
	s.webRoutes(mux)
	cancel := func(workflow bool) http.HandlerFunc {
		return StopHandler(func(id, reason string) error { return s.Cancel(id, workflow, reason) })
	}
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", cancel(false))
	mux.HandleFunc("POST /v1/workflows/{id}/cancel", cancel(true))
	mux.HandleFunc("POST /v1/jobs/{id}/stop", StopHandler(s.Stop))
	mux.HandleFunc("POST /v1/workflows", submitRoute(caller, 8<<20, s.SubmitWorkflow))
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) { WriteJSON(w, http.StatusOK, s.List()) })
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := s.job(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		WriteJSON(w, http.StatusOK, j)
	})
	mux.HandleFunc("POST /v1/jobs", submitRoute(caller, 1<<20, s.Submit))
	if caller == Local {
		mux.HandleFunc("POST /v1/batches", s.serveBatch)
		mux.HandleFunc("GET /v1/jobs/{id}/result", s.serveResult)
	}
	return mux
}
