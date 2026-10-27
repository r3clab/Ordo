package server

import (
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
	index := len(job.Attempts) - 1
	if query := r.URL.Query().Get("attempt"); query != "" {
		var err error
		index, err = strconv.Atoi(query)
		if err != nil {
			http.Error(w, "invalid attempt", 400)
			return
		}
	}
	dir := job.resultDir(index)
	if dir == "" {
		http.Error(w, "logs are available after collection", http.StatusConflict)
		return
	}
	file := filepath.Join(dir, name)
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() {
		http.Error(w, "log is unavailable", 500)
		return
	}
	f, err := os.Open(file)
	if err != nil {
		http.Error(w, "log is unavailable", 500)
		return
	}
	defer func() { _ = f.Close() }()
	const limit int64 = 1 << 20
	if info.Size() > limit {
		if _, err := f.Seek(-limit, io.SeekEnd); err != nil {
			http.Error(w, "log seek failed", 500)
			return
		}
		w.Header().Set("X-Ordo-Truncated", "true")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.Copy(w, io.LimitReader(f, limit))
}
