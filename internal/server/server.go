package server

import (
	"cmp"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/state"
	"github.com/r3clab/Ordo/internal/task"
	"github.com/r3clab/Ordo/internal/worker"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

type Request struct {
	ID        string            `json:"id,omitempty"`
	Image     string            `json:"image"`
	Archive   string            `json:"archive,omitempty"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Resources worker.Resources  `json:"resources"`
	task.Sandbox
	Secrets        map[string]string `json:"secrets,omitempty"`
	Services       []string          `json:"services,omitempty"`
	TimeoutSeconds int64             `json:"timeout_seconds,omitempty"`
	Retry          RetryPolicy       `json:"retry,omitempty"`
	Match          *worker.Match     `json:"match,omitempty"`
}
type Attempt struct {
	ID        string          `json:"id"`
	Worker    string          `json:"worker"`
	AgentID   string          `json:"agent_id"`
	Seen      bool            `json:"seen"`
	Remote    *worker.Attempt `json:"remote,omitempty"`
	ResultDir string          `json:"result_dir,omitempty"`
	Failure   string          `json:"failure,omitempty"`
}
type Job struct {
	ID              string            `json:"id"`
	Spec            worker.Spec       `json:"spec"`
	Archive         string            `json:"archive"`
	State           string            `json:"state"`
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
}

func (j Job) Terminal() bool {
	return j.State == "succeeded" || j.State == "failed" || j.State == "cancelled" || j.State == "skipped"
}

type Server struct {
	mu       sync.Mutex
	Root     string
	store    *state.Store
	workers  []worker.Remote
	services map[string]string
	jobs     map[string]Job
	busy     map[string]bool
	fault    error
	nodes    map[string]WorkerStatus
}

func Open(root string, workers []worker.Remote, services map[string]string) (*Server, error) {
	if len(workers) == 0 || len(workers) > 10 {
		return nil, errors.New("configure 1-10 workers")
	}
	seen := map[string]bool{}
	for _, w := range workers {
		if seen[w.Host] || w.Host == "" {
			return nil, errors.New("worker SSH aliases must be unique")
		}
		seen[w.Host] = true
		for name, address := range services {
			if _, err := w.ServiceForward(name, address); err != nil {
				return nil, err
			}
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	db, err := state.Open(root)
	if err != nil {
		return nil, err
	}
	s := &Server{Root: root, store: db, workers: workers, services: maps.Clone(services), jobs: map[string]Job{}, busy: map[string]bool{}, nodes: map[string]WorkerStatus{}}
	rows, err := db.List("job")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, row := range rows {
		var j Job
		if err := json.Unmarshal(row, &j); err != nil {
			_ = db.Close()
			return nil, err
		}
		if !worker.ValidName(j.ID) {
			_ = db.Close()
			return nil, errors.New("invalid saved job ID")
		}
		if !j.Terminal() {
			if len(j.Attempts) > 0 && !seen[j.Attempts[len(j.Attempts)-1].Worker] {
				_ = db.Close()
				return nil, errors.New("configuration removes a worker with unfinished attempts")
			}
			for _, name := range j.Spec.Services {
				if _, ok := services[name]; !ok {
					_ = db.Close()
					return nil, fmt.Errorf("configuration removes service %s, which unfinished job %s uses", name, j.ID)
				}
			}
		}
		s.jobs[j.ID] = j
	}
	// Secrets outlive their job when a crash or failed write falls between the two records.
	secrets, err := db.IDs("secret")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	for _, id := range secrets {
		if j, ok := s.jobs[id]; !ok || j.Terminal() {
			if err := db.Delete("secret", id); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	}
	return s, nil
}
func (s *Server) Close() error  { return s.store.Close() }
func Socket(root string) string { return filepath.Join(root, "server.sock") }
func (s *Server) job(id string) (Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	j.Attempts = slices.Clone(j.Attempts)
	return j, ok
}
func (s *Server) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		j.Attempts = slices.Clone(j.Attempts)
		jobs = append(jobs, j)
	}
	slices.SortFunc(jobs, func(a, b Job) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return jobs
}
func (s *Server) save(j Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshLocked(&j)
	return s.commitLocked(j)
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
	if j.CancelRequested && j.Terminal() {
		j.State = "cancelled"
	}
}

func (j Job) stopKind() string {
	switch {
	case j.CancelRequested:
		return "cancel"
	case j.StopRequested:
		return "stop"
	}
	return ""
}

// commitLocked latches s.fault on a failed write so no later mutation diverges from the store.
func (s *Server) commitLocked(jobs ...Job) error {
	if s.fault != nil {
		return s.fault
	}
	values := make(map[string]Job, len(jobs))
	for _, j := range jobs {
		values[j.ID] = j
	}
	if err := state.PutMany(s.store, "job", values); err != nil {
		s.fault = err
		return err
	}
	for _, j := range jobs {
		j.Attempts = slices.Clone(j.Attempts)
		s.jobs[j.ID] = j
	}
	for _, j := range jobs {
		if j.Terminal() && len(j.Spec.Secrets) > 0 {
			if err := s.store.Delete("secret", j.ID); err != nil {
				s.fault = err
				return err
			}
		}
	}
	return nil
}

type stagedImage struct{ id, blob string }

type staging struct {
	images map[[2]string]stagedImage
	inputs map[string]string
}

func newStaging() *staging {
	return &staging{images: map[[2]string]stagedImage{}, inputs: map[string]string{}}
}

func (s *Server) prepare(ctx context.Context, r Request, st *staging) (Job, error) {
	if r.Match != nil {
		if err := r.Match.Validate(r.Resources); err != nil {
			return Job{}, err
		}
	}
	if err := r.Retry.validate(); err != nil {
		return Job{}, err
	}
	if r.TimeoutSeconds < 0 || r.TimeoutSeconds > worker.MaxTimeoutSeconds {
		return Job{}, fmt.Errorf("timeout must be 0..%d seconds", worker.MaxTimeoutSeconds)
	}
	if err := r.Validate(r.Secrets); err != nil {
		return Job{}, err
	}
	services := slices.Sorted(slices.Values(r.Services))
	for i, name := range services {
		if _, ok := s.services[name]; !ok || (i > 0 && services[i-1] == name) {
			return Job{}, fmt.Errorf("service %q is unknown or repeated; configure it with ordo server --service", name)
		}
	}
	if r.ID == "" {
		r.ID = strings.ToLower(rand.Text())
	}
	if !worker.ValidName(r.ID) || r.Resources.Cores <= 0 || r.Resources.MemoryMiB <= 0 || r.Resources.GPUs < 0 {
		return Job{}, errors.New("valid ID and positive cores/memory required")
	}
	tmp, err := os.MkdirTemp(s.Root, ".prepare-")
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	blobs := filepath.Join(s.Root, "blobs")
	key := [2]string{r.Image, r.Archive}
	image, ok := st.images[key]
	if !ok {
		image.id = r.Image
		archive := r.Archive
		if archive == "" {
			archive = filepath.Join(tmp, "image.tar")
			image.id, err = task.ExportImage(ctx, image.id, archive)
			if err != nil {
				return Job{}, err
			}
		} else if !task.ValidImageID(image.id) {
			return Job{}, errors.New("an OCI archive requires its immutable sha256 image ID")
		}
		image.blob, err = content.PutFile(blobs, archive, "")
		if err != nil {
			return Job{}, err
		}
		st.images[key] = image
	}
	spec := worker.Spec{Image: image.id, Args: slices.Clone(r.Args), Resources: r.Resources, Inputs: map[string]string{}, TimeoutSeconds: r.TimeoutSeconds, Match: r.Match,
		Sandbox: task.Sandbox{Env: maps.Clone(r.Env), Network: r.Network, CapAdd: slices.Sorted(slices.Values(r.CapAdd))}, Secrets: slices.Sorted(maps.Keys(r.Secrets)), Services: services}
	for name, source := range r.Inputs {
		if !worker.ValidName(name) {
			return Job{}, errors.New("invalid input name")
		}
		hash, ok := st.inputs[source]
		if !ok {
			file := filepath.Join(tmp, "input.tar")
			hash, err = content.Archive(source, file)
			if err != nil {
				return Job{}, err
			}
			if _, err := content.PutFile(blobs, file, hash); err != nil {
				return Job{}, err
			}
			st.inputs[source] = hash
		}
		spec.Inputs[name] = hash
	}
	return Job{ID: r.ID, Spec: spec, Archive: image.blob, State: "queued", CreatedAt: time.Now().UTC(), Retry: r.Retry}, nil
}
func (s *Server) Submit(ctx context.Context, r Request) (Job, error) {
	j, err := s.prepare(ctx, r, newStaging())
	if err != nil {
		return Job{}, err
	}
	j.Key = jobKey(j)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return Job{}, s.fault
	}
	if old, ok := s.jobs[j.ID]; ok {
		key := old.Key
		if key == "" {
			key = jobKey(old)
		}
		if key != j.Key {
			return Job{}, errors.New("job ID already has a different immutable specification")
		}
		return old, nil
	}
	// Secrets live apart from jobs, which the API and UI return in full.
	if len(r.Secrets) > 0 {
		if err := s.store.Put("secret", j.ID, r.Secrets); err != nil {
			return Job{}, err
		}
	}
	if err := s.commitLocked(j); err != nil {
		return Job{}, err
	}
	return j, nil
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
	listener, err := worker.ListenUnix(Socket(s.Root))
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
func (s *Server) work(ctx context.Context, remote worker.Remote) {
	var tunnel *worker.Tunnel
	defer func() {
		if tunnel != nil {
			_ = tunnel.Close()
		}
	}()
	for ctx.Err() == nil {
		if tunnel == nil {
			probe, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err := worker.Bootstrap(probe, remote)
			if err == nil {
				tunnel, err = worker.Connect(ctx, remote, s.services)
			}
			cancel()
			if err != nil {
				s.workerStatus(remote.Host, nil, err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
				continue
			}
		}
		var info worker.Info
		err := worker.Call(ctx, tunnel.Client, http.MethodGet, "/v1/info", nil, &info)
		if err == nil {
			s.workerStatus(remote.Host, &info, nil)
			err = s.reconcileAll(ctx, remote.Host, info, tunnel.Client)
		}
		if err != nil {
			s.workerStatus(remote.Host, nil, err)
			_ = tunnel.Close()
			tunnel = nil
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
func (s *Server) reconcileAll(ctx context.Context, host string, info worker.Info, client *http.Client) error {
	for _, j := range s.List() {
		if j.Terminal() {
			continue
		}
		s.mu.Lock()
		j = s.jobs[j.ID]
		j.Attempts = slices.Clone(j.Attempts)
		available := !s.busy[j.ID] && s.fault == nil && !j.Terminal()
		if available {
			s.busy[j.ID] = true
		}
		s.mu.Unlock()
		if !available {
			continue
		}
		err := s.reconcile(ctx, j, host, info, client)
		s.mu.Lock()
		delete(s.busy, j.ID)
		s.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) reconcile(ctx context.Context, j Job, host string, info worker.Info, client *http.Client) error {
	if j.State == "queued" {
		if j.stopKind() != "" {
			j.State = "cancelled"
			return s.save(j)
		}
		ready, err := s.dependencies(&j)
		if err != nil || !ready {
			return err
		}
		if info.Error != "" || !worker.FitsSpec(info.Hardware, j.Spec, info.Held) {
			return nil
		}
		if err := s.importOutputs(&j); err != nil {
			return err
		}
		if err := worker.EnsureImage(ctx, client, filepath.Join(s.Root, "blobs", j.Archive), j.Archive, j.Spec.Image); err != nil {
			return err
		}
		for _, hash := range j.Spec.Inputs {
			if err := worker.EnsureInput(ctx, client, filepath.Join(s.Root, "blobs", hash), hash); err != nil {
				return err
			}
		}
		j.Attempts = append(j.Attempts, Attempt{ID: strings.ToLower(rand.Text()), Worker: host, AgentID: info.ID})
		j.State = "dispatching"
		if err := s.save(j); err != nil {
			return err
		}
	}
	at := &j.Attempts[len(j.Attempts)-1]
	if at.Worker != host {
		return nil
	}
	if at.AgentID != info.ID {
		j.State = "blocked"
		j.Error = "worker identity changed; reconcile the original attempt before retrying"
		return s.save(j)
	}
	var run worker.Attempt
	err := worker.Call(ctx, client, http.MethodGet, "/v1/attempts/"+at.ID, nil, &run)
	var api *worker.HTTPError
	if errors.As(err, &api) && api.Status == http.StatusNotFound {
		if at.Seen {
			j.State = "blocked"
			j.Error = "worker lost an acknowledged attempt"
			return s.save(j)
		}
		s.refresh(&j)
		if j.stopKind() != "" {
			if err := stopAttempt(ctx, client, at.ID, "cancel", j.StopReason); err != nil {
				return err
			}
			return s.finish(j, "cancel")
		}
		submission := worker.Submission{Spec: j.Spec}
		if len(j.Spec.Secrets) > 0 {
			err := s.store.Get("secret", j.ID, &submission.Secrets)
			if errors.Is(err, sql.ErrNoRows) {
				at.Failure = "infrastructure"
				j.State = "failed"
				j.Error = "job secrets are missing; resubmit it under a new ID"
				return s.save(j)
			}
			if err != nil {
				return err
			}
		}
		err = worker.Call(ctx, client, http.MethodPut, "/v1/attempts/"+at.ID, submission, &run)
		if errors.As(err, &api) && api.Status == http.StatusTooManyRequests {
			j.Attempts = j.Attempts[:len(j.Attempts)-1]
			j.State = "queued"
			return s.save(j)
		}
	}
	if err != nil {
		return err
	}
	unchanged := at.Seen && at.Remote != nil && at.Remote.State == run.State && at.Remote.UpdatedAt.Equal(run.UpdatedAt) && j.Error == run.Error
	at.Seen = true
	at.Remote = &run
	j.Error = run.Error
	// A stop sent to the worker directly still ends the job, as a server-side stop does.
	if run.StopRequested == "stop" {
		j.StopRequested = true
		j.StopReason = cmp.Or(j.StopReason, run.StopReason)
	}
	if !run.Terminal() {
		if kind := j.stopKind(); kind != "" && run.StopRequested == "" {
			if err := stopAttempt(ctx, client, at.ID, kind, j.StopReason); err != nil {
				return err
			}
		}
		state := "running"
		if run.State == "blocked" {
			state = "blocked"
		}
		if unchanged && j.State == state {
			return nil
		}
		j.State = state
		return s.save(j)
	}
	if run.State != "finished" && (run.Result == nil || run.Result.State != "finished") {
		class := "infrastructure"
		if run.StopRequested == "timeout" {
			class = "task"
		} else if run.State == "cancelled" {
			class = "cancel"
		}
		return s.finish(j, class)
	}
	if run.Result == nil || run.Result.ExitCode == nil {
		return errors.New("worker finished without a result")
	}
	j.State = "collecting"
	if err := s.save(j); err != nil {
		return err
	}
	dest := filepath.Join(s.Root, "results", j.ID, at.ID)
	if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
		if err := worker.FetchResult(ctx, client, at.ID, dest); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	result, err := task.ReadResult(dest)
	if err != nil {
		return err
	}
	if result.Request.ReservationID != worker.ReservationID(info.ID, at.ID) || result.ContainerID != run.Result.ContainerID || result.ImageID != j.Spec.Image || result.ExitCode == nil || *result.ExitCode != *run.Result.ExitCode {
		return errors.New("downloaded result does not match the dispatched attempt")
	}
	at.ResultDir = dest
	class := ""
	if *result.ExitCode != 0 || run.StopRequested == "timeout" {
		class = "task"
	}
	if run.State == "cancelled" && run.StopRequested != "timeout" {
		class = "cancel"
	}
	return s.finish(j, class)
}
func stopAttempt(ctx context.Context, client *http.Client, id, kind, reason string) error {
	return worker.Call(ctx, client, http.MethodPost, "/v1/attempts/"+id+"/"+kind, worker.StopRequest{Reason: reason}, nil)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.webRoutes(mux)
	cancel := func(workflow bool) http.HandlerFunc {
		return worker.StopHandler(func(id, reason string) error { return s.Cancel(id, workflow, reason) }, 500)
	}
	mux.HandleFunc("POST /v1/jobs/{id}/cancel", cancel(false))
	mux.HandleFunc("POST /v1/workflows/{id}/cancel", cancel(true))
	mux.HandleFunc("POST /v1/jobs/{id}/stop", worker.StopHandler(s.Stop, 500))
	mux.HandleFunc("POST /v1/workflows", func(w http.ResponseWriter, r *http.Request) {
		var flow Workflow
		if err := worker.DecodeJSON(http.MaxBytesReader(w, r.Body, 8<<20), &flow); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		jobs, err := s.SubmitWorkflow(r.Context(), flow)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		worker.WriteJSON(w, http.StatusOK, jobs)
	})
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) { worker.WriteJSON(w, http.StatusOK, s.List()) })
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, ok := s.job(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		worker.WriteJSON(w, http.StatusOK, j)
	})
	mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		if err := worker.DecodeJSON(http.MaxBytesReader(w, r.Body, 1<<20), &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		j, err := s.Submit(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		worker.WriteJSON(w, http.StatusOK, j)
	})
	return mux
}
