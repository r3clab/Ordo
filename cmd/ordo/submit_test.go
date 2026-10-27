package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func testServer(t *testing.T) (string, *imagetest.Nix) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "server")
	fake := &imagetest.Nix{Output: "/nix/store/aaaa-image.json", Config: "sha256:" + strings.Repeat("a", 64)}
	s, err := server.Open(server.Config{Root: root, Workers: []worker.Config{{Host: "worker", StateDir: "/srv/ordo", CPUs: "0-3", MemoryMiB: 1024}}, Tools: image.Tools{Exec: fake.Exec}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	serveSocket(t, root, s.Handler())
	return root, fake
}

func runWith(t *testing.T, input string, args ...string) ([]byte, error) {
	t.Helper()
	stdin = strings.NewReader(input)
	defer func() { stdin = os.Stdin }()
	var out bytes.Buffer
	err := run(context.Background(), args, &out)
	return out.Bytes(), err
}

func TestSubmitResolvesPathsAndSkipsDeliveredJobs(t *testing.T) {
	root, fake := testServer(t)
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.MkdirAll(filepath.Join(dir, "results", "done"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "results", "done", "score"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := `"image":"/src#image","resources":{"cores":1,"memory_mib":128}`
	jobs := strings.Join([]string{
		`{"out":"results/a",` + job + `}`,
		``,
		`{"out":"results/b","inputs":{"data":"."},` + job + `}`,
		`{"out":"results/done",` + job + `}`,
		`{"out":"results/a/nested",` + job + `}`,
		`not json`,
	}, "\n")
	out, err := runWith(t, jobs, "submit", "--state", root)
	results := decodeLines[server.BatchResult](t, out)
	if err == nil || err.Error() != "2 of 5 lines failed to submit" || len(results) != 5 {
		t.Fatalf("submit: %+v %v", results, err)
	}
	if r := results[0]; r.Line != 1 || r.Out != filepath.Join(dir, "results", "a") || r.State != server.Queued {
		t.Fatalf("relative out not resolved here: %+v", r)
	}
	if r := results[1]; r.Line != 3 || r.Error != "" {
		t.Fatalf("relative input not resolved here: %+v", r)
	}
	if r := results[2]; !r.Done || r.ID != "" || fake.Builds.Load() != 1 {
		t.Fatalf("a delivered out must be done without building: %+v, %d builds", r, fake.Builds.Load())
	}
	if r := results[3]; !strings.Contains(r.Error, "nests") {
		t.Fatalf("accepted an out inside another job's: %+v", r)
	}
	again, _ := runWith(t, jobs, "submit", "--state", root)
	if results := decodeLines[server.BatchResult](t, again); !results[0].Existing || results[0].ID != decodeLines[server.BatchResult](t, out)[0].ID {
		t.Fatalf("resubmission created a new job: %+v", results)
	}

	got, err := runWith(t, "", "queue", "get", "--state", root, "--out", "results/a")
	var job0 server.Job
	if err != nil || json.Unmarshal(got, &job0) != nil || job0.Out != filepath.Join(dir, "results", "a") {
		t.Fatalf("get by out: %s %v", got, err)
	}
	cancelled, err := runWith(t, "", "queue", "cancel", "--state", root, "--under", "results")
	if err != nil || strings.Count(string(cancelled), "\n") != 2 {
		t.Fatalf("cancel under: %s %v", cancelled, err)
	}
	listed, err := runWith(t, "", "queue", "list", "--state", root, "--under", filepath.Join(dir, "results", "b"))
	if jobs := decodeLines[[]server.Summary](t, listed); err != nil || len(jobs) != 1 || len(jobs[0]) != 1 || jobs[0][0].State != server.Cancelled {
		t.Fatalf("list under: %s %v", listed, err)
	}
}
