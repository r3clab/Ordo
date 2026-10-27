package image

import (
	"context"
	"os"
	"path/filepath"

	"github.com/r3clab/Ordo/internal/content"
)

type Origin struct {
	Installable string `json:"installable,omitempty"`
	Output      string `json:"output,omitempty"`
	Reference   string `json:"reference,omitempty"`
}

type Entry struct{ ID, Blob string }

type Resolved struct {
	Entry
	Origin *Origin
}

// Cache maps a nix store path or a digest-pinned registry reference to its archive; both are immutable,
// so entries never go stale.
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
	return r.flight.do(ctx, string(src), func() (Resolved, error) {
		select {
		case r.builds <- struct{}{}:
		case <-r.ctx.Done():
			return Resolved{}, r.ctx.Err()
		}
		defer func() { <-r.builds }()
		dir, err := os.MkdirTemp(r.Scratch, ".image-")
		if err != nil {
			return Resolved{}, err
		}
		defer func() { _ = os.RemoveAll(dir) }()
		if src.Registry() {
			return r.pull(r.ctx, string(src), dir)
		}
		return r.nix(r.ctx, string(src), dir)
	})
}

func (r *Resolver) nix(ctx context.Context, installable, dir string) (Resolved, error) {
	// The out-link is a GC root that keeps the output alive until skopeo has read it.
	out, err := r.Tools.Build(ctx, installable, filepath.Join(dir, "result"))
	if err != nil {
		return Resolved{}, err
	}
	origin := &Origin{Installable: installable, Output: out}
	return r.archive(out, origin, filepath.Join(dir, "image.tar"), func(file string) error { return r.Tools.Export(ctx, out, file) })
}

// pull caches by the reference itself, which its digest makes immutable.
func (r *Resolver) pull(ctx context.Context, reference, dir string) (Resolved, error) {
	origin := &Origin{Reference: reference}
	return r.archive(reference, origin, filepath.Join(dir, "image.tar"), func(file string) error { return r.Tools.Pull(ctx, reference, file) })
}

// archive stores the OCI archive that export writes to file, unless the cache already holds key's.
func (r *Resolver) archive(key string, origin *Origin, file string, export func(file string) error) (Resolved, error) {
	e, ok, err := r.Cache.Load(key)
	if err != nil || (ok && content.Has(r.Blobs, e.Blob)) {
		return Resolved{Entry: e, Origin: origin}, err
	}
	if err := export(file); err != nil {
		return Resolved{}, err
	}
	if e.ID, err = ArchiveID(file); err != nil {
		return Resolved{}, err
	}
	if e.Blob, err = content.PutFile(r.Blobs, file, ""); err != nil {
		return Resolved{}, err
	}
	return Resolved{Entry: e, Origin: origin}, r.Cache.Save(key, e)
}
