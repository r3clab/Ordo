package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/image/imagetest"
	"github.com/r3clab/Ordo/internal/worker"
)

func nixServer(t *testing.T) (*Server, *imagetest.Nix) {
	t.Helper()
	fake := testNix()
	s, err := Open(Config{Root: filepath.Join(t.TempDir(), "server"), Workers: []worker.Config{testWorker("worker")}, Tools: image.Tools{Exec: fake.Exec}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, fake
}

func TestBatchSharesNixImagesAndContinuesPastBadLines(t *testing.T) {
	s, fake := nixServer(t)
	resources := `"resources":{"cores":1,"memory_mib":128}`
	batch := strings.Join([]string{
		`{"id":"a","image":{"nix":"/src#grep"},` + resources + `}`,
		``,
		`{"id":"b","image":{"nix":"/src#grep"},"args":["x"],` + resources + `}`,
		`not json`,
		`{"image":{"nix":"/src#grep"},` + resources + `}`,
		`{"id":"c","image":{"nix":"/src#grep"},"color":"red",` + resources + `}`,
		`{"id":"a","image":{"nix":"/src#grep"},"args":["changed"],` + resources + `}`,
		`{"id":"d","image":{"nix":"/src#never"},"resources":{"cores":0,"memory_mib":128}}`,
	}, "\n")
	submit := func() []BatchResult {
		entries, err := ParseBatch(strings.NewReader(batch))
		if err != nil {
			t.Fatal(err)
		}
		return slices.Collect(s.SubmitBatch(context.Background(), entries))
	}
	results := submit()
	var lines, failed []int
	for _, result := range results {
		lines = append(lines, result.Line)
		if result.Error != "" {
			failed = append(failed, result.Line)
		}
	}
	if !slices.Equal(lines, []int{1, 3, 4, 5, 6, 7, 8}) || !slices.Equal(failed, []int{4, 5, 6, 7, 8}) || results[0].State != "queued" || results[0].Existing {
		t.Fatalf("results %+v", results)
	}
	if fake.Builds.Load() != 1 || fake.Exports.Load() != 1 {
		t.Fatalf("batch built %d and exported %d times", fake.Builds.Load(), fake.Exports.Load())
	}
	j, _ := s.job("a")
	if j.Spec.Image != testImageID || j.ImageOrigin == nil || j.ImageOrigin.Installable != "/src#grep" || j.ImageOrigin.Output != "/nix/store/aaaa-image.json" {
		t.Fatalf("job image %+v %+v", j.Spec.Image, j.ImageOrigin)
	}
	again := submit()
	if !again[0].Existing || !again[1].Existing || fake.Exports.Load() != 1 {
		t.Fatalf("resubmission must reuse jobs and the exported image: %+v exports=%d", again, fake.Exports.Load())
	}
}

func TestBrowserRoutesCannotSubmit(t *testing.T) {
	s, fake := nixServer(t)
	for _, test := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/v1/jobs", `{"id":"a","image":{"nix":"/src#grep"},"resources":{"cores":1,"memory_mib":128}}`, http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/workflows", `{"id":"w","tasks":[{"id":"t","image":{"nix":"/src#grep"},"resources":{"cores":1,"memory_mib":128}}]}`, http.StatusNotFound},
		{http.MethodPost, "/v1/batches", `{"id":"a","image":{"nix":"/src#grep"},"resources":{"cores":1,"memory_mib":128}}`, http.StatusNotFound},
		{http.MethodGet, "/v1/jobs/a/result", ``, http.StatusNotFound},
	} {
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
		if w.Code != test.want {
			t.Fatalf("%s %s: HTTP %d %s", test.method, test.path, w.Code, w.Body)
		}
	}
	if fake.Builds.Load() != 0 {
		t.Fatal("a browser request ran nix")
	}
}

func TestResultIsOutputsBesideLogs(t *testing.T) {
	s, _ := nixServer(t)
	dir := filepath.Join(s.Root, "results", "done", "attempt")
	for name, data := range map[string]string{"run.json": "{}", "stdout.log": "out", "stderr.log": "err", "outputs.tar": "tar", "outputs/klee/info": "stats"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, j := range []Job{
		{ID: "done", State: "succeeded", Attempts: []Attempt{{ID: "attempt", ResultDir: dir}}},
		{ID: "running", State: "running", Attempts: []Attempt{{ID: "attempt"}}},
	} {
		if err := s.save(j); err != nil {
			t.Fatal(err)
		}
	}
	client := handlerClient(t, s.Handler())
	into := t.TempDir()
	if err := Download(context.Background(), client, "/v1/jobs/done/result", filepath.Join(into, "done")); err != nil {
		t.Fatal(err)
	}
	var got []string
	if err := filepath.WalkDir(filepath.Join(into, "done"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			rel, _ := filepath.Rel(filepath.Join(into, "done"), path)
			got = append(got, rel)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"klee/info", "run.json", "stderr.log", "stdout.log"}) {
		t.Fatalf("fetched %q", got)
	}
	var api *HTTPError
	if err := Download(context.Background(), client, "/v1/jobs/running/result", filepath.Join(into, "running")); !errors.As(err, &api) || api.Status != http.StatusConflict {
		t.Fatalf("unfinished job: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outputs", "stdout.log"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Download(context.Background(), client, "/v1/jobs/done/result", filepath.Join(into, "again")); !errors.As(err, &api) || api.Status != http.StatusConflict {
		t.Fatalf("colliding output: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "outputs", "stdout.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "outputs", "a")); err != nil {
		t.Fatal(err)
	}
	if err := Download(context.Background(), client, "/v1/jobs/done/result", filepath.Join(into, "linked")); !errors.As(err, &api) || api.Status != http.StatusInternalServerError {
		t.Fatalf("an output the archive refuses must fail with an error response: %v", err)
	}
}
