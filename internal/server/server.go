package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/state"
	"github.com/r3clab/Ordo/internal/task"
	"github.com/r3clab/Ordo/internal/worker"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type Request struct {
	ID        string            `json:"id,omitempty"`
	Image     string            `json:"image"`
	Archive   string            `json:"archive,omitempty"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Resources worker.Resources  `json:"resources"`
}
type Attempt struct {
	ID        string          `json:"id"`
	Worker    string          `json:"worker"`
	AgentID   string          `json:"agent_id"`
	Seen      bool            `json:"seen"`
	Remote    *worker.Attempt `json:"remote,omitempty"`
	ResultDir string          `json:"result_dir,omitempty"`
}
type Job struct {
	ID        string      `json:"id"`
	Spec      worker.Spec `json:"spec"`
	Archive   string      `json:"archive"`
	State     string      `json:"state"`
	Error     string      `json:"error,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	Attempts  []Attempt   `json:"attempts,omitempty"`
}

func (j Job) Terminal() bool {
	return j.State == "succeeded" || j.State == "failed" || j.State == "cancelled" || j.State == "skipped"
}

type Server struct {
	mu      sync.Mutex
	Root    string
	store   *state.Store
	workers []worker.Remote
	jobs    map[string]Job
	busy    map[string]bool
	fault   error
}

func Open(root string, workers []worker.Remote) (*Server, error) {
	if len(workers) == 0 || len(workers) > 10 {
		return nil, errors.New("configure 1-10 workers")
	}
	seen := map[string]bool{}
	for _, w := range workers {
		if seen[w.Host] || w.Host == "" {
			return nil, errors.New("worker SSH aliases must be unique")
		}
		seen[w.Host] = true
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	db, err := state.Open(root)
	if err != nil {
		return nil, err
	}
	s := &Server{Root: root, store: db, workers: workers, jobs: map[string]Job{}, busy: map[string]bool{}}
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
		if !namePattern.MatchString(j.ID) {
			_ = db.Close()
			return nil, errors.New("invalid saved job ID")
		}
		if !j.Terminal() && len(j.Attempts) > 0 && !seen[j.Attempts[len(j.Attempts)-1].Worker] {
			_ = db.Close()
			return nil, errors.New("configuration removes a worker with unfinished attempts")
		}
		s.jobs[j.ID] = j
	}
	return s, nil
}
func (s *Server) Close() error { return s.store.Close() }
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
	if s.fault != nil {
		return s.fault
	}
	if err := s.store.Put("job", j.ID, j); err != nil {
		s.fault = err
		return err
	}
	j.Attempts = slices.Clone(j.Attempts)
	s.jobs[j.ID] = j
	return nil
}
func (s *Server) Submit(ctx context.Context, r Request) (Job, error) {
	if r.ID == "" {
		r.ID = strings.ToLower(rand.Text())
	}
	if !namePattern.MatchString(r.ID) || r.Resources.Cores <= 0 || r.Resources.MemoryMiB <= 0 {
		return Job{}, errors.New("valid ID and positive cores/memory required")
	}
	tmp, err := os.MkdirTemp(s.Root, ".prepare-")
	if err != nil {
		return Job{}, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	image := r.Image
	archive := r.Archive
	if archive == "" {
		archive = filepath.Join(tmp, "image.tar")
		image, err = task.ExportImage(ctx, image, archive)
		if err != nil {
			return Job{}, err
		}
	} else if !strings.HasPrefix(image, "sha256:") || !content.Valid(strings.TrimPrefix(image, "sha256:")) {
		return Job{}, errors.New("an OCI archive requires its immutable sha256 image ID")
	}
	file, err := os.Open(archive)
	if err != nil {
		return Job{}, err
	}
	blob, err := content.Put(filepath.Join(s.Root, "blobs"), file, "")
	err = errors.Join(err, file.Close())
	if err != nil {
		return Job{}, err
	}
	spec := worker.Spec{Image: image, Args: slices.Clone(r.Args), Resources: r.Resources, Inputs: map[string]string{}}
	for name, source := range r.Inputs {
		if !namePattern.MatchString(name) {
			return Job{}, errors.New("invalid input name")
		}
		file := filepath.Join(tmp, "input.tar")
		hash, err := content.Archive(source, file)
		if err != nil {
			return Job{}, err
		}
		f, err := os.Open(file)
		if err != nil {
			return Job{}, err
		}
		_, err = content.Put(filepath.Join(s.Root, "blobs"), f, hash)
		err = errors.Join(err, f.Close())
		if err != nil {
			return Job{}, err
		}
		spec.Inputs[name] = hash
	}
	j := Job{ID: r.ID, Spec: spec, Archive: blob, State: "queued", CreatedAt: time.Now().UTC()}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fault != nil {
		return Job{}, s.fault
	}
	if old, ok := s.jobs[j.ID]; ok {
		left, _ := json.Marshal(old.Spec)
		right, _ := json.Marshal(spec)
		if string(left) != string(right) || old.Archive != blob {
			return Job{}, errors.New("job ID already has a different immutable specification")
		}
		return old, nil
	}
	if err := s.store.Put("job", j.ID, j); err != nil {
		s.fault = err
		return Job{}, err
	}
	s.jobs[j.ID] = j
	return j, nil
}

func (s *Server) Run(ctx context.Context) error {
	socket := filepath.Join(s.Root, "server.sock")
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("server socket path is not a socket")
		}
		if c, err := net.DialTimeout("unix", socket, time.Second); err == nil {
			_ = c.Close()
			return errors.New("server socket already live")
		}
		if err := os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, remote := range s.workers {
		wg.Go(func() { s.work(child, remote) })
	}
	api := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- api.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err = <-done:
	}
	cancel()
	shutdown, end := context.WithTimeout(context.Background(), 5*time.Second)
	defer end()
	err = errors.Join(err, api.Shutdown(shutdown))
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
				tunnel, err = worker.Connect(ctx, remote)
			}
			cancel()
			if err != nil {
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
				err = s.reconcile(ctx, j, remote.Host, info, tunnel.Client)
				s.mu.Lock()
				delete(s.busy, j.ID)
				s.mu.Unlock()
				if err != nil {
					break
				}
			}
		}
		if err != nil {
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
func (s *Server) reconcile(ctx context.Context, j Job, host string, info worker.Info, client *http.Client) error {
	if len(j.Attempts) == 0 {
		if info.Error != "" || !worker.Fits(info.Hardware, j.Spec.Resources, info.Held) {
			return nil
		}
		if err := worker.EnsureImage(ctx, client, filepath.Join(s.Root, "blobs", j.Archive), j.Spec.Image); err != nil {
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
		err = worker.Call(ctx, client, http.MethodPut, "/v1/attempts/"+at.ID, j.Spec, &run)
		if errors.As(err, &api) && api.Status == http.StatusTooManyRequests {
			j.Attempts = j.Attempts[:len(j.Attempts)-1]
			j.State = "queued"
			return s.save(j)
		}
	}
	if err != nil {
		return err
	}
	at.Seen = true
	at.Remote = &run
	j.Error = run.Error
	if !run.Terminal() {
		j.State = "running"
		if run.State == "blocked" {
			j.State = "blocked"
		}
		return s.save(j)
	}
	if run.State != "finished" {
		j.State = "failed"
		return s.save(j)
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
	if result.Request.ReservationID != info.ID+"/"+at.ID || result.ContainerID != run.Result.ContainerID || result.ImageID != j.Spec.Image || result.ExitCode == nil || *result.ExitCode != *run.Result.ExitCode {
		return errors.New("downloaded result does not match the dispatched attempt")
	}
	at.ResultDir = dest
	j.State = "succeeded"
	if *result.ExitCode != 0 {
		j.State = "failed"
	}
	return s.save(j)
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.List()) })
	mux.HandleFunc("GET /v1/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		for _, j := range s.List() {
			if j.ID == r.PathValue("id") {
				writeJSON(w, j)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("POST /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		var req Request
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		d.DisallowUnknownFields()
		if err := d.Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		j, err := s.Submit(r.Context(), req)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		writeJSON(w, j)
	})
	return mux
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
