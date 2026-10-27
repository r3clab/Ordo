package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/image/imagetest"
	"github.com/r3clab/Ordo/internal/server"
	"github.com/r3clab/Ordo/internal/worker"
)

func serveSocket(t *testing.T, root string, handler http.Handler) {
	t.Helper()
	listener, err := server.ListenUnix(server.Socket(root))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func decodeLines[T any](t *testing.T, data []byte) []T {
	t.Helper()
	var out []T
	decoder := json.NewDecoder(bytes.NewReader(data))
	for decoder.More() {
		var value T
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
		out = append(out, value)
	}
	return out
}

func TestSubmitStreamsResultsAndIsIdempotent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "server")
	fake := &imagetest.Nix{Output: "/nix/store/aaaa-image.json", Config: "sha256:" + strings.Repeat("a", 64)}
	s, err := server.Open(server.Config{Root: root, Workers: []worker.Config{{Host: "worker", StateDir: "/srv/ordo", CPUs: "0-3", MemoryMiB: 1024}}, Tools: image.Tools{Exec: fake.Exec}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	serveSocket(t, root, s.Handler())
	job := `"image":"/src#image","resources":{"cores":1,"memory_mib":128}`
	jobs := `{"id":"a",` + job + "}\n" + `{"id":"b",` + job + "}\n" + `{"id":"bad id",` + job + "}\n" + `{"id":"flow","tasks":[{"id":"t",` + job + "}]}\n"
	submit := func() ([]server.BatchResult, error) {
		stdin = strings.NewReader(jobs)
		defer func() { stdin = os.Stdin }()
		var out bytes.Buffer
		err := run(context.Background(), []string{"submit", "--state", root}, &out)
		return decodeLines[server.BatchResult](t, out.Bytes()), err
	}
	results, err := submit()
	if err == nil || err.Error() != "1 of 4 lines failed to submit" || len(results) != 4 || results[0].State != "queued" || results[2].Error == "" || !results[3].Workflow || results[3].Error != "" || results[3].Existing {
		t.Fatalf("submit: %+v %v", results, err)
	}
	again, _ := submit()
	if !again[0].Existing || !again[1].Existing || !again[3].Existing {
		t.Fatalf("resubmission created new jobs: %+v", again)
	}
}

func TestFetchWaitsAndSkipsExistingDirectories(t *testing.T) {
	result := t.TempDir()
	for name, data := range map[string]string{"run.json": "{}", "stdout.log": "out", "stderr.log": "err", "outputs/klee/info": "stats"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(result, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(result, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var polls atomic.Int32
	var repolled atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		state := server.Running
		if polls.Add(1) > 1 {
			state = server.Succeeded
			repolled.Store(r.URL.Query()["id"])
		}
		server.WriteJSON(w, 200, []server.Summary{{ID: "a", State: state}, {ID: "b", State: "succeeded"}, {ID: "c", State: "failed"}})
	})
	mux.HandleFunc("GET /v1/jobs/{id}/result", func(w http.ResponseWriter, r *http.Request) {
		var tar bytes.Buffer
		if err := content.Tar(&tar, content.Part{Path: filepath.Join(result, "outputs")}, content.Part{Path: filepath.Join(result, "run.json"), Name: "run.json"}); err != nil {
			t.Error(err)
		}
		sum := sha256.Sum256(tar.Bytes())
		w.Header().Set(server.ChecksumHeader, hex.EncodeToString(sum[:]))
		_, _ = w.Write(tar.Bytes())
	})
	root := t.TempDir()
	serveSocket(t, root, mux)
	into := filepath.Join(t.TempDir(), "results")
	if err := os.MkdirAll(filepath.Join(into, "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	pollInterval = time.Millisecond
	defer func() { pollInterval = 2 * time.Second }()
	var out bytes.Buffer
	err := run(context.Background(), []string{"fetch", "--state", root, "--into", into, "--wait"}, &out)
	got := decodeLines[fetchResult](t, out.Bytes())
	statuses := map[string]string{}
	for _, r := range got {
		statuses[r.ID] = r.Status
	}
	if err == nil || len(got) != 3 || statuses["a"] != "fetched" || statuses["b"] != "exists" || statuses["c"] != "unsuccessful" || got[2].ID != "a" {
		t.Fatalf("fetch: %+v %v", got, err)
	}
	if ids, _ := repolled.Load().([]string); !slices.Equal(ids, []string{"a"}) {
		t.Fatalf("a wait must poll only the pending jobs, not %q", ids)
	}
	if data, err := os.ReadFile(filepath.Join(into, "a", "klee", "info")); err != nil || string(data) != "stats" {
		t.Fatalf("outputs not at the job directory's root: %q %v", data, err)
	}
	out.Reset()
	if err := run(context.Background(), []string{"fetch", "--state", root, "--into", into, "--id", "a", "--id", "b"}, &out); err != nil {
		t.Fatalf("refetch: %v %s", err, out.String())
	}
	if err := run(context.Background(), []string{"fetch", "--state", root, "--into", into, "--id", "zzz"}, &out); err == nil {
		t.Fatal("fetched an unknown job")
	}
}
