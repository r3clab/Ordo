package worker

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
)

const protocol = 1
const maxControl = 64 << 20
const resultTrailer = "Ordo-Result"

type initialization struct {
	Protocol int
	Config   Config
	Server   string
	Services map[string]string
}

type request struct {
	Attempt  string
	Previous string
	ID       string
	Launch   Launch
	GPUs     []pool.GPU
	Keep     []string
	Idle     time.Duration
}

type result struct {
	Error string `json:"error,omitempty"`
	Exit  *int   `json:"exit,omitempty"`
}

func outcome(err error) result {
	if err == nil {
		return result{}
	}
	if exit, ok := errors.AsType[*ssh.ExitError](err); ok {
		return result{Error: exit.Stderr, Exit: &exit.Code}
	}
	return result{Error: err.Error()}
}
func (r result) err() error {
	if r.Exit != nil {
		return &ssh.ExitError{Code: *r.Exit, Stderr: r.Error}
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}

// Serve is the internal worker entry point. Its only durable state is the existing worker tree.
// stdin is a lifetime lease, not an RPC channel: EOF cancels requests and closes the API.
func Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	reader := bufio.NewReader(io.LimitReader(input, (1<<20)+1))
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if len(line) > 1<<20 {
		return errors.New("worker initialization is too large")
	}
	var init initialization
	if err := json.Unmarshal([]byte(line), &init); err != nil {
		return err
	}
	if init.Protocol != protocol {
		return errors.New("worker protocol mismatch")
	}
	if err := init.Config.Validate(); err != nil {
		return err
	}
	if !spec.ValidName(init.Server) {
		return errors.New("invalid server identity")
	}
	h := New(init.Config, process{}, init.Server)
	if err := os.MkdirAll(h.StateDir, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(h.StateDir)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s must be mode 700", h.StateDir)
	}
	if err := prepareServices(ctx, h, init.Services); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "ordo-worker-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	socket := filepath.Join(dir, "api")
	if len(socket) > maxSocketPath {
		return errors.New("worker temporary directory makes the API socket path too long")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _, _ = io.Copy(io.Discard, reader); cancel() }()
	api := newAPI(h)
	var mu sync.Mutex
	var active sync.WaitGroup
	stopping := false
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			if stopping {
				mu.Unlock()
				http.Error(w, "worker shutting down", http.StatusServiceUnavailable)
				return
			}
			active.Add(1)
			mu.Unlock()
			defer active.Done()
			api.ServeHTTP(w, r)
		}),
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(listener) }()
	if _, err = fmt.Fprintln(output, socket); err != nil {
		cancel()
	}
	select {
	case <-ctx.Done():
	case err = <-done:
		cancel()
	}
	mu.Lock()
	stopping = true
	mu.Unlock()
	_ = srv.Close()
	active.Wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func prepareServices(ctx context.Context, h *Host, services map[string]string) error {
	prepare := "set -eu; umask 077"
	for name, address := range services {
		if _, err := h.ServiceForward(name, address); err != nil {
			return err
		}
		prepare += "; mkdir -p -- " + ssh.Quote(h.serviceDir(name)) + "; rm -f -- " + ssh.Quote(h.serviceSocket(name))
	}
	if len(services) == 0 {
		return nil
	}
	return h.run.Run(ctx, prepare, nil, nil, nil)
}

func newAPI(h *Host) http.Handler {
	// Transfers cannot occupy the control plane; requests get independent forwarded connections.
	transfers := make(chan struct{}, 2)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rpc/{operation}", func(w http.ResponseWriter, r *http.Request) {
		var q request
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxControl))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&q); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var extra any
		if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, errors.New("expected one request"))
			return
		}
		value, err := h.control(r.Context(), r.PathValue("operation"), q)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	})
	stream := func(w http.ResponseWriter, r *http.Request, run func() error) {
		select {
		case transfers <- struct{}{}:
			defer func() { <-transfers }()
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Trailer", resultTrailer)
		w.WriteHeader(http.StatusOK)
		err := run()
		data, _ := json.Marshal(outcome(err))
		w.Header().Set(resultTrailer, base64.StdEncoding.EncodeToString(data))
	}
	mux.HandleFunc("PUT /{kind}/{id}", func(w http.ResponseWriter, r *http.Request) {
		kind, id := r.PathValue("kind"), r.PathValue("id")
		if (kind == "images" && spec.ValidImageID(id)) || (kind == "inputs" && content.Valid(id)) || (kind == "seed" && spec.ValidName(id)) {
			r.Body = http.MaxBytesReader(w, r.Body, content.MaxSize)
			stream(w, r, func() error {
				switch kind {
				case "images":
					return h.importImage(r.Context(), r.Body, id)
				case "inputs":
					return receiveTree(r.Context(), h.treeDir(id), r.Body, id)
				default:
					return receiveTree(r.Context(), path.Join(h.runDir(id), OutputsDir), r.Body, "")
				}
			})
			return
		}
		writeError(w, http.StatusBadRequest, errors.New("invalid upload target"))
	})
	mux.HandleFunc("GET /runs/{attempt}/{file}", func(w http.ResponseWriter, r *http.Request) {
		attempt, file := r.PathValue("attempt"), r.PathValue("file")
		if !spec.ValidName(attempt) || !slices.Contains([]string{StdoutFile, StderrFile, OutputsDir}, file) {
			writeError(w, http.StatusBadRequest, errors.New("invalid collection target"))
			return
		}
		stream(w, r, func() error {
			switch file {
			case StdoutFile:
				return h.logs(r.Context(), attempt, w, nil)
			case StderrFile:
				return h.logs(r.Context(), attempt, nil, w)
			default:
				return h.outputs(r.Context(), attempt, w)
			}
		})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "ordo-worker" || r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(outcome(err))
}

func (h *Host) control(ctx context.Context, operation string, q request) (any, error) {
	switch operation {
	case "probe":
		return h.Probe(ctx)
	case "poll":
		return h.Poll(ctx)
	case "gpu":
		return h.GPUBusy(ctx, q.GPUs)
	case "image":
		if !spec.ValidImageID(q.ID) {
			return nil, errors.New("invalid image identity")
		}
		return h.cached(q.ID, func() (bool, error) {
			absent, err := h.absent(ctx, prelude()+"pm image exists -- "+ssh.Quote(q.ID), 1)
			return !absent, err
		})
	case "input":
		if !content.Valid(q.ID) {
			return nil, errors.New("invalid input identity")
		}
		return h.cached(q.ID, func() (bool, error) {
			absent, err := h.absent(ctx, "test -d "+ssh.Quote(h.treeDir(q.ID)), 1)
			return !absent, err
		})
	case "launch":
		if err := h.validateLaunch(q.Launch); err != nil {
			return nil, err
		}
		return h.Launch(ctx, q.Launch)
	case "prune":
		if q.Idle < time.Minute {
			return nil, errors.New("invalid prune age")
		}
		for _, id := range q.Keep {
			if !content.Valid(id) && !spec.ValidImageID(id) {
				return nil, errors.New("invalid retained content identity")
			}
		}
		return nil, h.Prune(ctx, q.Keep, q.Idle)
	}
	if !spec.ValidName(q.Attempt) {
		return nil, errors.New("invalid attempt identity")
	}
	switch operation {
	case "seed":
		if !spec.ValidName(q.Previous) {
			return nil, errors.New("invalid previous attempt identity")
		}
		return h.seed(ctx, q.Attempt, q.Previous)
	case "stop":
		return nil, h.Stop(ctx, q.Attempt)
	case "inspect":
		return h.Inspect(ctx, q.Attempt)
	case "remove":
		return nil, h.Remove(ctx, q.Attempt)
	case "release":
		return nil, h.Release(ctx, q.Attempt)
	default:
		return nil, errors.New("unknown worker operation")
	}
}

func (h *Host) validateLaunch(l Launch) error {
	if !spec.ValidName(l.Attempt) || !spec.ValidImageID(l.Spec.Image) {
		return errors.New("invalid launch identity")
	}
	if err := l.Spec.Validate(); err != nil {
		return err
	}
	for name, id := range l.Spec.Inputs {
		if !spec.ValidName(name) || !content.Valid(id) {
			return errors.New("invalid launch input")
		}
	}
	for name, value := range l.Secrets {
		if err := spec.CheckSecret(name, value); err != nil {
			return err
		}
	}
	for _, service := range l.Spec.Services {
		if !spec.ValidName(service) {
			return errors.New("invalid service name")
		}
	}
	if len(l.Allocation.CPUs) != l.Spec.Resources.Cores || l.Allocation.MemoryMiB != l.Spec.Resources.MemoryMiB || len(l.Allocation.GPUs) != l.Spec.Resources.GPUs || !h.Fits(l.Spec.Resources) {
		return errors.New("allocation does not match resources")
	}
	cpus := h.cpus()
	seen := map[int]bool{}
	for _, cpu := range l.Allocation.CPUs {
		if seen[cpu] || !slices.Contains(cpus, cpu) {
			return errors.New("invalid CPU allocation")
		}
		seen[cpu] = true
	}
	if !spec.ValidGPUSet(l.Allocation.GPUs) {
		return errors.New("invalid GPU allocation")
	}
	for _, gpu := range l.Allocation.GPUs {
		if !slices.Contains(h.GPUs, gpu) {
			return errors.New("GPU is not configured")
		}
	}
	for _, arg := range slices.Concat(l.Spec.Args, l.Spec.Entrypoint) {
		if strings.ContainsRune(arg, 0) {
			return errors.New("command contains NUL")
		}
	}
	return nil
}
