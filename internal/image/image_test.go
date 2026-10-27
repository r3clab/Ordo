package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/image/imagetest"
)

var config = "sha256:" + strings.Repeat("c", 64)

func ociArchive(t *testing.T, file string, files map[string]string) {
	t.Helper()
	if err := imagetest.WriteArchive(file, files); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveID(t *testing.T) {
	good := imagetest.Layout(imagetest.Manifest(config))
	twoManifests := imagetest.Layout(`{}`)
	twoManifests["index.json"] = `{"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:1"},{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:2"}]}`
	nested := imagetest.Layout(`{}`)
	nested["index.json"] = strings.Replace(nested["index.json"], "manifest.v1", "index.v1", 1)
	for name, test := range map[string]struct {
		files map[string]string
		want  string
	}{
		"single manifest": {good, config},
		"two manifests":   {twoManifests, ""},
		"nested index":    {nested, ""},
		"bad config":      {imagetest.Layout(`{"config":{"digest":"sha256:short"}}`), ""},
		"missing blob":    {map[string]string{"index.json": good["index.json"]}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "image.tar")
			ociArchive(t, file, test.files)
			got, err := ArchiveID(file)
			if test.want == "" && err == nil || test.want != "" && (err != nil || got != test.want) {
				t.Fatalf("ArchiveID = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestSourceValidate(t *testing.T) {
	for _, test := range []struct {
		src Source
		ok  bool
	}{
		{Source{Nix: "/src#x"}, true},
		{Source{}, false},
		{Source{Nix: "--option"}, false},
		{Source{Nix: "a\nb"}, false},
		{Source{Nix: strings.Repeat("x", MaxInstallableBytes+1)}, false},
	} {
		if err := test.src.Validate(); (err == nil) != test.ok {
			t.Fatalf("%+v: %v", test.src, err)
		}
	}
}

type memoryCache struct {
	mu      sync.Mutex
	entries map[string]Entry
}

func (c *memoryCache) Load(output string) (Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[output]
	return e, ok, nil
}
func (c *memoryCache) Save(output string, e Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[output] = e
	return nil
}

func fakeNix() *imagetest.Nix {
	return &imagetest.Nix{Output: "/nix/store/aaaa-image.json", Config: config}
}

func newResolver(t *testing.T, fake *imagetest.Nix) *Resolver {
	t.Helper()
	root := t.TempDir()
	r := NewResolver(filepath.Join(root, "blobs"), root, Tools{Exec: fake.Exec}, &memoryCache{entries: map[string]Entry{}})
	t.Cleanup(r.Close)
	return r
}

func TestResolverCachesNixExportsByStorePath(t *testing.T) {
	fake := fakeNix()
	r := newResolver(t, fake)
	ctx := context.Background()
	src := Source{Nix: "/src#images.grep"}
	first, err := r.Resolve(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != config || first.Origin == nil || first.Origin.Output != "/nix/store/aaaa-image.json" {
		t.Fatalf("resolved %+v", first)
	}
	build := fake.Argv("nix")
	if len(build) != 8 || !slices.Equal(build[:4], []string{"--extra-experimental-features", "nix-command flakes", "build", "--out-link"}) || !strings.HasPrefix(build[4], r.Scratch) || !slices.Equal(build[5:], []string{"--print-out-paths", "--", "/src#images.grep"}) {
		t.Fatalf("nix argv %q", build)
	}
	if export := fake.Argv("skopeo"); !slices.Equal(export[:3], []string{"--insecure-policy", "copy", "nix:/nix/store/aaaa-image.json"}) {
		t.Fatalf("skopeo argv %q", export)
	}
	again, err := r.Resolve(ctx, src)
	if err != nil || again.Blob != first.Blob || fake.Exports.Load() != 1 || fake.Builds.Load() != 2 {
		t.Fatalf("cache hit must skip skopeo: %+v %v exports=%d", again, err, fake.Exports.Load())
	}
	if err := os.Remove(filepath.Join(r.Blobs, first.Blob)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(ctx, src); err != nil || fake.Exports.Load() != 2 {
		t.Fatalf("a lost blob must be exported again: %v exports=%d", err, fake.Exports.Load())
	}
}

func TestResolverSharesBuildsAndBoundsThem(t *testing.T) {
	fake := fakeNix()
	fake.Release = make(chan struct{})
	r := newResolver(t, fake)
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Resolve(gone, Source{Nix: "/src#a"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter: %v", err)
	}
	var wg sync.WaitGroup
	for i := range 3 * builds {
		wg.Go(func() {
			if _, err := r.Resolve(context.Background(), Source{Nix: fmt.Sprintf("/src#%c", 'a'+i%(2*builds))}); err != nil {
				t.Error(err)
			}
		})
	}
	deadline := time.Now().Add(5 * time.Second)
	for fake.Builds.Load() < builds && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if got := fake.Builds.Load(); got != builds {
		t.Fatalf("%d builds ran at once; the limit is %d", got, builds)
	}
	close(fake.Release)
	wg.Wait()
	if got := fake.Builds.Load(); got != 2*builds {
		t.Fatalf("%d builds for %d distinct installables", got, 2*builds)
	}
}
