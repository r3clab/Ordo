package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if w.Code != 200 || w.Body.Len() != 1<<20+4 {
		t.Fatal("whole log read failed", w.Code)
	}
	tail := httptest.NewRequest("GET", "http://127.0.0.1:7331/v1/jobs/logs/logs/stdout", nil)
	tail.Header.Set("Range", "bytes=-4")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, tail)
	if w.Code != 206 || w.Body.String() != "tail" || w.Header().Get("Content-Range") != "bytes 1048576-1048579/1048580" {
		t.Fatal("ranged log read failed", w.Code, w.Header())
	}
	if err := os.WriteFile(filepath.Join(dir, "stderr.log"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	tail = httptest.NewRequest("GET", "http://127.0.0.1:7331/v1/jobs/logs/logs/stderr", nil)
	tail.Header.Set("Range", "bytes=-4")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, tail)
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Range") != "" {
		t.Fatal("ranged read of an empty log failed", w.Code, w.Header())
	}
	w = request("GET", "/v1/jobs/logs/logs/server.db", "", "", nil)
	if w.Code != 400 {
		t.Fatal("arbitrary file access accepted")
	}
	if w = request("GET", "/v1/jobs/logs/logs/stdout?attempt=1", "", "", nil); w.Code != 404 {
		t.Fatal("a missing attempt must be not found", w.Code)
	}
}

func TestFollowStreamsFromTheWorkerThenTheCollectedLog(t *testing.T) {
	f := newFixture(t)
	h := f.s.Handler()
	id := f.submit(t, jobRequest(""))
	follow := func(query string) (w *httptest.ResponseRecorder, aborted bool) {
		defer func() {
			if p := recover(); p != nil {
				if p != http.ErrAbortHandler {
					panic(p)
				}
				aborted = true
			}
		}()
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "http://ordo/v1/jobs/"+id+"/logs/stdout?follow=1"+query, nil))
		return w, false
	}
	get := func(query string) *httptest.ResponseRecorder {
		t.Helper()
		w, aborted := follow(query)
		if aborted {
			t.Fatalf("follow%s aborted", query)
		}
		return w
	}
	f.h.refuse = errors.New("not yet")
	f.tick(t)
	if w := get(""); w.Code != 409 {
		t.Fatalf("an attempt without a container must not be followed: %d", w.Code)
	}
	ready(t, f.s, id)
	f.tick(t)
	at := last(f.job(t, id))
	if w := get(""); w.Code != 503 {
		t.Fatalf("a follow needs the worker's connection: %d %s", w.Code, w.Body)
	}
	ctx, cancel := context.WithCancel(context.Background())
	unlink := f.s.link(ctx, "worker", f.h)
	f.h.live[at.ID+"/"+worker.StdoutFile] = "live output"
	if w := get("&offset=5"); w.Code != 200 || w.Body.String() != "output" {
		t.Fatalf("a resumed follow must skip what it has: %d %q", w.Code, w.Body)
	}
	f.h.hold = make(chan struct{})
	ended := make(chan bool, 1)
	go func() { _, aborted := follow(""); ended <- aborted }()
	cancel()
	if !<-ended {
		t.Fatal("the session's end must cut its follows off")
	}
	unlink()
	f.h.hold, f.h.cut = nil, errors.New("connection lost")
	f.s.link(context.Background(), "worker", f.h)
	if _, aborted := follow(""); !aborted {
		t.Fatal("a follow cut off after it began must abort the response")
	}
	f.h.live = map[string]string{}
	if w := get(""); w.Code != 502 {
		t.Fatalf("a follow that fails before any output must say so: %d", w.Code)
	}
	f.h.exit(at.ID, 0)
	f.tick(t)
	if w := get("&offset=1"); w.Code != 200 || w.Body.String() != "ut" {
		t.Fatalf("a collected attempt must be followed from its log: %d %q", w.Code, w.Body)
	}
}

func TestJobListConditionalRequests(t *testing.T) {
	f := newFixture(t)
	h := f.s.WebHandler()
	get := func(method, path, etag string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://127.0.0.1:7331"+path, nil)
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	initial := get("GET", "/v1/jobs", "")
	etag := initial.Header().Get("ETag")
	if initial.Code != 200 || etag == "" || initial.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("initial list: %d, %v", initial.Code, initial.Header())
	}
	for _, match := range []string{etag, "W/" + etag, `"other", W/` + etag, "*"} {
		for _, method := range []string{"GET", "HEAD"} {
			w := get(method, "/v1/jobs", match)
			if w.Code != 304 || w.Body.Len() != 0 || w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s %s: %d, %v, %s", method, match, w.Code, w.Header(), w.Body.String())
			}
		}
	}
	if w := get("GET", "/v1/jobs", `"different"`); w.Code != 200 || w.Body.String() != initial.Body.String() {
		t.Fatalf("nonmatching validator: %d, %s", w.Code, w.Body.String())
	}
	changed := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		w := get("GET", path, etag)
		if w.Code != 200 || w.Header().Get("ETag") == "" || w.Header().Get("ETag") == etag {
			t.Fatalf("changed list: %d, %v, %s", w.Code, w.Header(), w.Body.String())
		}
		etag = w.Header().Get("ETag")
		return w
	}
	id := f.submit(t, jobRequest(f.out("job")))
	changed("/v1/jobs")
	f.s.wait(id, "worker", "insufficient cores")
	w := changed("/v1/jobs")
	if !strings.Contains(w.Body.String(), "insufficient cores") {
		t.Fatal("pending reason missing", w.Body.String())
	}
	f.s.wait(id, "worker", "insufficient cores")
	if w := get("GET", "/v1/jobs", etag); w.Code != 304 {
		t.Fatal("unchanged pending reason invalidated the list")
	}
	f.s.wait(id, "worker", "insufficient memory")
	changed("/v1/jobs")
	f.s.forget("worker")
	changed("/v1/jobs")
	changed("/v1/jobs?state=succeeded")
	changed("/v1/jobs?id=" + id)
	if err := f.s.Cancel(id, ""); err != nil {
		t.Fatal(err)
	}
	w = changed("/v1/jobs")
	if !strings.Contains(w.Body.String(), `"state":"cancelled"`) {
		t.Fatal("cancelled state missing", w.Body.String())
	}
	if err := f.s.expire(time.Now().Add(retention + time.Hour)); err != nil {
		t.Fatal(err)
	}
	changed("/v1/jobs")
}

func TestJobListETagIsStableWithEqualCreationTimes(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	seed(t, f.s, Job{ID: "b", Out: f.out("b"), State: Queued, CreatedAt: now}, Job{ID: "a", Out: f.out("a"), State: Queued, CreatedAt: now})
	h := f.s.Handler()
	var etag string
	for range 20 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/v1/jobs", nil))
		if w.Code != 200 || w.Header().Get("ETag") == "" || etag != "" && w.Header().Get("ETag") != etag {
			t.Fatalf("unstable list: %d, %v", w.Code, w.Header())
		}
		etag = w.Header().Get("ETag")
		var jobs []Summary
		if err := json.Unmarshal(w.Body.Bytes(), &jobs); err != nil || len(jobs) != 2 || jobs[0].ID != "a" || jobs[1].ID != "b" {
			t.Fatalf("unstable order: %s, %v", w.Body.String(), err)
		}
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
