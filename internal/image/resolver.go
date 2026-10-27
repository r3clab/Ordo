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
			// The reference's digest makes it immutable, so it is its own cache key.
			return r.archive(string(src), string(src), &Origin{Reference: string(src)}, dir)
		}
		// The out-link is a GC root that keeps the output alive until skopeo has read it.
		out, err := r.Tools.Build(r.ctx, string(src), filepath.Join(dir, "result"))
		if err != nil {
			return Resolved{}, err
		}
		return r.archive(out, "nix:"+out, &Origin{Installable: string(src), Output: out}, dir)
	})
}

// archive stores the OCI archive of src, unless the cache already holds key's.
func (r *Resolver) archive(key, src string, origin *Origin, dir string) (Resolved, error) {
	e, ok, err := r.Cache.Load(key)
	if err != nil || (ok && content.Reuse(r.Blobs, e.Blob)) {
		return Resolved{Entry: e, Origin: origin}, err
	}
	file := filepath.Join(dir, "image.tar")
	if err := r.Tools.Copy(r.ctx, src, file); err != nil {
		return Resolved{}, err
	}
	if e.ID, err = ArchiveID(file); err != nil {
		return Resolved{}, err
	}
	if e.Blob, err = content.MoveFile(r.Blobs, file); err != nil {
		return Resolved{}, err
	}
	return Resolved{Entry: e, Origin: origin}, r.Cache.Save(key, e)
}
