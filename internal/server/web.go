package server

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/r3clab/Ordo/internal/content"
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
	status.Host = host
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
	result := make([]WorkerStatus, 0, len(s.workers))
	for _, cfg := range s.workers {
		status := s.nodes[cfg.Host]
		status.Host = cfg.Host
		status.Tags = cfg.Tags
		result = append(result, status)
	}
	return result
}
func (s *Server) Token() (string, error) {
	file := filepath.Join(s.Root, "token")
	info, err := os.Lstat(file)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return "", errors.New("token must be a private regular mode-0600 file")
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(data))
		if !content.Valid(token) {
			return "", errors.New("invalid saved token")
		}
		return token, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var secret [32]byte
	_, _ = rand.Read(secret[:])
	token := hex.EncodeToString(secret[:])
	err = content.WriteAtomic(file, func(w io.Writer) error {
		_, err := io.WriteString(w, token+"\n")
		return err
	})
	if err != nil {
		return "", err
	}
	return token, nil
}
func listenWeb(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, errors.New("web listen address must be a literal loopback IP")
	}
	return net.Listen("tcp", address)
}
func (s *Server) WebHandler(token string) http.Handler {
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
		ip := net.ParseIP(host)
		if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "loopback host required", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
			http.Error(w, "cross-origin request denied", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			supplied, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || len(token) != 64 || subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
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
	job, ok := s.job(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
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
	dir, err := job.resultDir(index)
	if err != nil {
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
