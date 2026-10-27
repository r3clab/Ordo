package server

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	dir := t.TempDir()
	resources := `"resources":{"cores":1,"memory_mib":128}`
	out := func(name string) string { return `"out":"` + filepath.Join(dir, name) + `",` }
	batch := strings.Join([]string{
		`{` + out("a") + `"image":"/src#grep",` + resources + `}`,
		``,
		`{` + out("b") + `"steps":[{"image":"/src#grep",` + resources + `},{"name":"report","image":"/src#grep","args":["x"],` + resources + `}]}`,
		`not json`,
		`{"out":"relative","image":"/src#grep",` + resources + `}`,
		`{` + out("c") + `"image":"/src#grep","color":"red",` + resources + `}`,
		`{` + out("a") + `"image":"/src#grep","args":["changed"],` + resources + `}`,
		`{` + out("d") + `"image":"/src#never","resources":{"cores":0,"memory_mib":128}}`,
		`{` + out("e") + `"image":"/src#grep",` + resources + `,"steps":[{"image":"/src#grep",` + resources + `}]}`,
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
	if !slices.Equal(lines, []int{1, 3, 4, 5, 6, 7, 8, 9}) || !slices.Equal(failed, []int{4, 5, 6, 7, 8, 9}) || results[0].State != Queued || results[0].Existing {
		t.Fatalf("results %+v", results)
	}
	if fake.Builds.Load() != 1 || fake.Exports.Load() != 1 {
		t.Fatalf("batch built %d and exported %d times", fake.Builds.Load(), fake.Exports.Load())
	}
	j := mustJob(t, s, results[1].ID)
	if len(j.Steps) != 2 || j.Steps[1].Name != "report" || j.Steps[0].Spec.Image != testImageID || j.Steps[0].ImageOrigin == nil || j.Steps[0].ImageOrigin.Installable != "/src#grep" || j.Steps[0].ImageOrigin.Output != "/nix/store/aaaa-image.json" {
		t.Fatalf("job steps %+v", j.Steps)
	}
	again := submit()
	if !again[0].Existing || !again[1].Existing || again[1].ID != results[1].ID || fake.Exports.Load() != 1 {
		t.Fatalf("resubmission must reuse jobs and the exported image: %+v exports=%d", again, fake.Exports.Load())
	}
}

func TestBrowserRoutesCannotSubmit(t *testing.T) {
	s, fake := nixServer(t)
	for _, test := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPost, "/v1/jobs", `{"out":"/tmp/a","image":"/src#grep","resources":{"cores":1,"memory_mib":128}}`, http.StatusMethodNotAllowed},
		{http.MethodPost, "/v1/batches", `{"out":"/tmp/a","image":"/src#grep","resources":{"cores":1,"memory_mib":128}}`, http.StatusNotFound},
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
