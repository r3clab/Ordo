package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/worker"
)

func TestWebCancellationAndLogs(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "server"), []worker.Config{testWorker("worker")}, nil)
	defer func() { _ = s.Close() }()
	h := s.WebHandler()
	request := func(method, path, host, origin string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:7331"+path, bytes.NewReader(body))
		if host != "" {
			r.Host = host
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/v1/jobs", "evil.example:7331", "", nil); w.Code != 403 {
		t.Fatal("DNS rebinding accepted")
	}
	if w := request("POST", "/v1/jobs/example/cancel", "", "http://evil.example", nil); w.Code != 403 {
		t.Fatal("cross-origin write accepted")
	}
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := request("GET", path, "", "", nil)
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("asset/security headers", path, w.Code)
		}
	}
	example, err := s.Submit(context.Background(), jobRequest(filepath.Join(t.TempDir(), "example")))
	if err != nil {
		t.Fatal(err)
	}
	w := request("POST", "/v1/jobs/"+example.ID+"/cancel", "", "http://127.0.0.1:7331", nil)
	if w.Code != 200 || !mustJob(t, s, example.ID).CancelRequested {
		t.Fatal("cancel failed", w.Body.String())
	}
	dir := filepath.Join(s.root, "result")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.log"), []byte(strings.Repeat("x", 1<<20)+"tail"), 0o600); err != nil {
		t.Fatal(err)
	}
	seed(t, s, Job{ID: "logs", State: "succeeded", Attempts: []Attempt{{ResultDir: dir}}})
	w = request("GET", "/v1/jobs/logs/logs/stdout", "", "", nil)
	if w.Code != 200 || w.Body.Len() != 1<<20 || !strings.HasSuffix(w.Body.String(), "tail") || w.Header().Get("X-Ordo-Truncated") != "true" {
		t.Fatal("bounded log read failed")
	}
	w = request("GET", "/v1/jobs/logs/logs/server.db", "", "", nil)
	if w.Code != 400 {
		t.Fatal("arbitrary file access accepted")
	}
}
func TestWebListenerIsLoopbackOnly(t *testing.T) {
	l, err := listenWeb(0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if addr := l.Addr().(*net.TCPAddr); !addr.IP.IsLoopback() {
		t.Fatal("unsafe bind", addr)
	}
}
func TestWebHTTPRoundTrip(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "server"), []worker.Config{testWorker("worker")}, nil)
	defer func() { _ = s.Close() }()
	srv := httptest.NewServer(s.WebHandler())
	defer srv.Close()
	response, err := srv.Client().Get(srv.URL + "/v1/workers")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(data), "worker") {
		t.Fatal(response.StatusCode, string(data))
	}
}
