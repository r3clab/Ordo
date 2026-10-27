package image

import (
	"context"
	"os"
	"path/filepath"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/podman"
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

// Cache maps an immutable image identity, a nix store path or a Podman image ID, to its exported
// archive, so entries never go stale.
type Cache interface {
	Load(key string) (Entry, bool, error)
	Save(key string, e Entry) error
}

const builds = 4

type Resolver struct {
	Blobs, Scratch string
	Tools          Tools
	Cache          Cache
	imageID        func(ctx context.Context, reference string) (string, error)
	save           func(ctx context.Context, id, dest string) error

	ctx    context.Context
	cancel context.CancelFunc
	flight flight[string, Resolved]
	builds chan struct{}
}

func NewResolver(blobs, scratch string, tools Tools, cache Cache) *Resolver {
	ctx, cancel := context.WithCancel(context.Background())
	return &Resolver{Blobs: blobs, Scratch: scratch, Tools: tools, Cache: cache, imageID: podman.ImageID, save: podman.SaveImage, ctx: ctx, cancel: cancel, builds: make(chan struct{}, builds)}
}

func (r *Resolver) Close() {
	r.cancel()
	r.flight.wait()
}

func (r *Resolver) Resolve(ctx context.Context, src Source) (Resolved, error) {
	switch {
	case src.IsNix():
		// A build outlives the request that started it, so a retry after a disconnect finds it cached.
		return r.flight.do(ctx, src.Nix, func() (Resolved, error) { return r.nix(r.ctx, src.Nix) })
	case src.Archive != "":
		blob, err := content.PutFile(r.Blobs, src.Archive, "")
		return Resolved{Entry: Entry{ID: src.ID, Blob: blob}}, err
	}
	id, err := r.imageID(ctx, src.Ref)
	if err != nil {
		return Resolved{}, err
	}
	e, err := r.export(id, func(file string) (string, error) { return id, r.save(ctx, id, file) })
	return Resolved{Entry: e}, err
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
	e, err := r.export(out, func(file string) (string, error) {
		if err := r.Tools.Export(ctx, out, file); err != nil {
			return "", err
		}
		return ArchiveID(file)
	})
	return Resolved{Entry: e, Origin: &Origin{Installable: installable, Output: out}}, err
}

func (r *Resolver) export(key string, write func(file string) (id string, err error)) (Entry, error) {
	e, ok, err := r.Cache.Load(key)
	if err != nil || (ok && content.Has(r.Blobs, e.Blob)) {
		return e, err
	}
	dir, err := os.MkdirTemp(r.Scratch, ".image-")
	if err != nil {
		return Entry{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	file := filepath.Join(dir, "image.tar")
	if e.ID, err = write(file); err != nil {
		return Entry{}, err
	}
	if e.Blob, err = content.PutFile(r.Blobs, file, ""); err != nil {
		return Entry{}, err
	}
	return e, r.Cache.Save(key, e)
}
