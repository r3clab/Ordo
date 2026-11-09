package server

import (
	"cmp"
	"context"
	"embed"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/worker"
)

//go:embed web/*
var webFiles embed.FS

// Content types are fixed rather than taken from the host's MIME tables, which could change them.
var webAssets = map[string]struct{ file, kind string }{
	"/":          {"index.html", "text/html; charset=utf-8"},
	"/app.js":    {"app.js", "text/javascript; charset=utf-8"},
	"/style.css": {"style.css", "text/css; charset=utf-8"},
}

var logStreams = map[string]string{"stdout": worker.StdoutFile, "stderr": worker.StderrFile}

type WorkerInfo struct {
	BootID   string            `json:"boot_id"`
	Hardware pool.Hardware     `json:"hardware"`
	Held     []pool.Allocation `json:"held"`
	Error    string            `json:"error,omitempty"`
}

type WorkerStatus struct {
	Host      string      `json:"host"`
	Connected bool        `json:"connected"`
	LastSeen  time.Time   `json:"last_seen"`
	Error     string      `json:"error,omitempty"`
	Info      *WorkerInfo `json:"info,omitempty"`
	Tags      []string    `json:"tags,omitempty"`
}

func (s *Server) workerStatus(host string, info *WorkerInfo, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.nodes[host]
	status.Connected = err == nil
	if err != nil {
		status.Error = err.Error()
	} else {
		status.Error = ""
		status.LastSeen = time.Now().UTC()
		status.Info = info
	}
	s.nodes[host] = status
}
func (s *Server) Workers() []WorkerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	workers := s.settings.Load().workers
	result := make([]WorkerStatus, 0, len(workers))
	for _, cfg := range workers {
		status := s.nodes[cfg.Host]
		status.Host = cfg.Host
		status.Tags = cfg.Tags
		result = append(result, status)
	}
	return result
}
func listenWeb(port uint16) (net.Listener, error) {
	return net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
}

func loopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// WebHandler has no authentication: the host is meant for one user, whose processes could read any
// credential the server kept, and the Host and Origin checks turn away other sites in that user's browser.
func (s *Server) WebHandler() http.Handler {
	api := s.routes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			http.Error(w, "invalid host", 400)
			return
		}
		if host != "localhost" && !loopback(host) {
			http.Error(w, "loopback host required", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "cross-origin request denied", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		asset, ok := webAssets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", asset.kind)
		http.ServeFileFS(w, r, webFiles, "web/"+asset.file)
	})
}
func (s *Server) serveLogs(w http.ResponseWriter, r *http.Request) {
	name, ok := logStreams[r.PathValue("stream")]
	if !ok {
		http.Error(w, "unknown log stream", 400)
		return
	}
	job, ok := s.lookup(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	index := len(job.Attempts) - 1
	if attempt := query.Get("attempt"); attempt != "" {
		var err error
		if index, err = strconv.Atoi(attempt); err != nil {
			http.Error(w, "invalid attempt", 400)
			return
		}
	}
	if index < 0 || index >= len(job.Attempts) {
		http.Error(w, "no such attempt", http.StatusNotFound)
		return
	}
	at := job.Attempts[index]
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if query.Get("follow") == "" {
		if at.ResultDir == "" {
			http.Error(w, "logs are available after collection, or to follow", http.StatusConflict)
			return
		}
		f, ok := openLog(w, filepath.Join(at.ResultDir, name))
		if !ok {
			return
		}
		defer func() { _ = f.Close() }()
		// ServeContent answers a suffix range of an empty file with the invalid Content-Range "bytes 0--1/0".
		if info, err := f.Stat(); err == nil && info.Size() == 0 {
			r.Header.Del("Range")
		}
		http.ServeContent(w, r, "", time.Time{}, f)
		return
	}
	offset, err := strconv.ParseInt(cmp.Or(query.Get("offset"), "0"), 10, 64)
	if err != nil || offset < 0 {
		http.Error(w, "invalid offset", 400)
		return
	}
	s.follow(w, r, job, index, name, offset)
}

func (s *Server) follow(w http.ResponseWriter, r *http.Request, job Job, index int, name string, offset int64) {
	dir := job.Attempts[index].ResultDir
	if dir == "" {
		if dir = s.followLive(w, r, job, index, name, offset); dir == "" {
			return
		}
	}
	f, ok := openLog(w, filepath.Join(dir, name))
	if !ok {
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		http.Error(w, "log seek failed", 500)
		return
	}
	_, _ = io.Copy(w, f)
}

// followLive answers with the log from the attempt's worker, unless it finds the attempt collected and
// returns where. Only an attempt that never started leaves a job, so index stays its own.
func (s *Server) followLive(w http.ResponseWriter, r *http.Request, job Job, index int, name string, offset int64) string {
	at := job.Attempts[index]
	if at.ContainerID == "" {
		http.Error(w, "attempt has not started", http.StatusConflict)
		return ""
	}
	s.mu.Lock()
	l, linked := s.links[at.Worker]
	s.mu.Unlock()
	if !linked {
		http.Error(w, "worker is not connected", http.StatusServiceUnavailable)
		return ""
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(l.ctx, cancel)()
	live := &skipper{w: worker.Flushing(w), skip: offset}
	err := l.host.Follow(ctx, at.ID, name, live)
	switch {
	case err == nil:
		return ""
	case live.sent:
		// A client must never take a cut-off log for a whole one.
		panic(http.ErrAbortHandler)
	}
	// The attempt may have been collected, and its container removed, before the follow began.
	if j, ok, _ := s.job(job.ID); ok && j.Attempts[index].ResultDir != "" {
		return j.Attempts[index].ResultDir
	}
	http.Error(w, "follow: "+err.Error(), http.StatusBadGateway)
	return ""
}

func openLog(w http.ResponseWriter, file string) (*os.File, bool) {
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "log is unavailable", 500)
		return nil, false
	}
	f, err := os.Open(file)
	if err != nil {
		http.Error(w, "log is unavailable", 500)
		return nil, false
	}
	return f, true
}

type skipper struct {
	w    io.Writer
	skip int64
	sent bool
}

func (s *skipper) Write(p []byte) (int, error) {
	n := len(p)
	if s.skip >= int64(n) {
		s.skip -= int64(n)
		return n, nil
	}
	p, s.skip, s.sent = p[s.skip:], 0, true
	if _, err := s.w.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}
