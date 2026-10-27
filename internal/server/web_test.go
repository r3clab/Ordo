package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/worker"
)

func TestWebAuthenticationCancellationAndLogs(t *testing.T) {
	s, err := Open(Config{Root: filepath.Join(t.TempDir(), "server"), Workers: []worker.Config{testWorker("worker")}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	token, err := s.Token()
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Token()
	if err != nil || again != token {
		t.Fatal("token not stable", err)
	}
	h := s.WebHandler(token)
	request := func(method, path, auth, host, origin string, body []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1:7331"+path, bytes.NewReader(body))
		if auth != "" {
			r.Header.Set("Authorization", "Bearer "+auth)
		}
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
	for _, path := range []string{"/v1/jobs", "/v1/workers", "/v1/jobs/example/logs/stdout"} {
		if w := request("GET", path, "", "", "", nil); w.Code != 401 {
			t.Fatal("unauthenticated access", path, w.Code)
		}
	}
	if w := request("GET", "/v1/jobs", "wrong", "", "", nil); w.Code != 401 {
		t.Fatal("bad token accepted")
	}
	if w := request("GET", "/v1/jobs", token, "evil.example:7331", "", nil); w.Code != 403 {
		t.Fatal("DNS rebinding accepted")
	}
	if w := request("POST", "/v1/jobs/example/cancel", token, "", "http://evil.example", nil); w.Code != 403 {
		t.Fatal("cross-origin write accepted")
	}
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		w := request("GET", path, "", "", "", nil)
		if w.Code != 200 || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("asset/security headers", path, w.Code)
		}
	}
	archive := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(archive, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(context.Background(), Request{ID: "example", Image: image.Source{Archive: archive, ID: "sha256:" + strings.Repeat("a", 64)}, Resources: spec.Resources{Cores: 1, MemoryMiB: 128}}); err != nil {
		t.Fatal(err)
	}
	w := request("POST", "/v1/jobs/example/cancel", token, "", "http://127.0.0.1:7331", nil)
	if w.Code != 200 || !s.List()[0].CancelRequested {
		t.Fatal("cancel failed", w.Body.String())
	}
	dir := filepath.Join(s.Root, "result")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.log"), []byte(strings.Repeat("x", 1<<20)+"tail"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.save(Job{ID: "logs", State: "succeeded", Attempts: []Attempt{{ResultDir: dir}}}); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/v1/jobs/logs/logs/stdout", token, "", "", nil)
	if w.Code != 200 || w.Body.Len() != 1<<20 || !strings.HasSuffix(w.Body.String(), "tail") || w.Header().Get("X-Ordo-Truncated") != "true" {
		t.Fatal("bounded log read failed")
	}
	w = request("GET", "/v1/jobs/logs/logs/token", token, "", "", nil)
	if w.Code != 400 {
		t.Fatal("arbitrary file access accepted")
	}
	if err := os.Chmod(filepath.Join(s.Root, "token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Token(); err == nil {
		t.Fatal("public token accepted")
	}
}
func TestWebListenerIsLoopbackOnly(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "example.com:0", "localhost:0"} {
		if l, err := listenWeb(address); err == nil {
			_ = l.Close()
			t.Fatal("unsafe bind", address)
		}
	}
	l, err := listenWeb("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = l.Close()
}
func TestWebHTTPRoundTrip(t *testing.T) {
	s, err := Open(Config{Root: filepath.Join(t.TempDir(), "server"), Workers: []worker.Config{testWorker("worker")}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	token, err := s.Token()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.WebHandler(token))
	defer srv.Close()
	r, _ := http.NewRequest("GET", srv.URL+"/v1/workers", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	response, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(data), "worker") {
		t.Fatal(response.StatusCode, string(data))
	}
}
