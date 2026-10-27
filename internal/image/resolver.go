package image

import (
	"context"
	"os"
	"path/filepath"

	"github.com/r3clab/Ordo/internal/content"
)

type Origin struct {
	Installable string `json:"installable"`
	Output      string `json:"output"`
}

type Entry struct{ ID, Blob string }

type Resolved struct {
	Entry
	Origin *Origin
}

// Cache maps a nix store path to its exported archive; store paths are immutable, so entries never
// go stale.
type Cache interface {
	Load(key string) (Entry, bool, error)
	Save(key string, e Entry) error
}

const builds = 4

type Resolver struct {
	Blobs, Scratch string
	Tools          Tools
	Cache          Cache

	ctx    context.Context
	cancel context.CancelFunc
	flight flight[string, Resolved]
	builds chan struct{}
}

func NewResolver(blobs, scratch string, tools Tools, cache Cache) *Resolver {
	ctx, cancel := context.WithCancel(context.Background())
	return &Resolver{Blobs: blobs, Scratch: scratch, Tools: tools, Cache: cache, ctx: ctx, cancel: cancel, builds: make(chan struct{}, builds)}
}

func (r *Resolver) Close() {
	r.cancel()
	r.flight.wait()
}

func (r *Resolver) Resolve(ctx context.Context, src Source) (Resolved, error) {
	// A build outlives the request that started it, so a retry after a disconnect finds it cached.
	return r.flight.do(ctx, string(src), func() (Resolved, error) { return r.nix(r.ctx, string(src)) })
}

func (r *Resolver) nix(ctx context.Context, installable string) (Resolved, error) {
	select {
	case r.builds <- struct{}{}:
	case <-ctx.Done():
		return Resolved{}, ctx.Err()
	}
	defer func() { <-r.builds }()
	dir, err := os.MkdirTemp(r.Scratch, ".image-")
	if err != nil {
		return Resolved{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	// The out-link is a GC root that keeps the output alive until skopeo has read it.
	out, err := r.Tools.Build(ctx, installable, filepath.Join(dir, "result"))
	if err != nil {
		return Resolved{}, err
	}
	origin := &Origin{Installable: installable, Output: out}
	e, ok, err := r.Cache.Load(out)
	if err != nil || (ok && content.Has(r.Blobs, e.Blob)) {
		return Resolved{Entry: e, Origin: origin}, err
	}
	file := filepath.Join(dir, "image.tar")
	if err := r.Tools.Export(ctx, out, file); err != nil {
		return Resolved{}, err
	}
	if e.ID, err = ArchiveID(file); err != nil {
		return Resolved{}, err
	}
	if e.Blob, err = content.PutFile(r.Blobs, file, ""); err != nil {
		return Resolved{}, err
	}
	return Resolved{Entry: e, Origin: origin}, r.Cache.Save(out, e)
}
