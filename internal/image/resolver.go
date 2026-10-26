package image

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/task"
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

// Cache maps a built store path to its exported image; store paths are immutable, so entries never go stale.
type Cache interface {
	Load(output string) (Entry, bool, error)
	Save(output string, e Entry) error
}

const builds = 4

type Resolver struct {
	Blobs, Scratch string
	Tools          Tools
	Cache          Cache
	podman         func(ctx context.Context, reference, dest string) (string, error)

	ctx    context.Context
	cancel context.CancelFunc
	flight flight[string, Resolved]
	builds chan struct{}
}

func NewResolver(blobs, scratch string, tools Tools, cache Cache) *Resolver {
	ctx, cancel := context.WithCancel(context.Background())
	return &Resolver{Blobs: blobs, Scratch: scratch, Tools: tools, Cache: cache, podman: task.ExportImage, ctx: ctx, cancel: cancel, builds: make(chan struct{}, builds)}
}

func (r *Resolver) Close() {
	r.cancel()
	r.flight.wait()
}

func (r *Resolver) Resolve(ctx context.Context, src Source, archive string) (Resolved, error) {
	switch {
	case src.IsNix():
		// A build outlives the request that started it, so a retry after a disconnect finds it cached.
		return r.flight.do(ctx, src.Nix, func() (Resolved, error) { return r.nix(r.ctx, src.Nix) })
	case archive != "":
		if !task.ValidImageID(src.Ref) {
			return Resolved{}, errors.New("an OCI archive requires its immutable sha256 image ID")
		}
		blob, err := content.PutFile(r.Blobs, archive, "")
		return Resolved{Entry: Entry{ID: src.Ref, Blob: blob}}, err
	}
	return r.scratch(func(dir string) (Resolved, error) {
		file := filepath.Join(dir, "image.tar")
		id, err := r.podman(ctx, src.Ref, file)
		if err != nil {
			return Resolved{}, err
		}
		blob, err := content.PutFile(r.Blobs, file, "")
		return Resolved{Entry: Entry{ID: id, Blob: blob}}, err
	})
}

func (r *Resolver) nix(ctx context.Context, installable string) (Resolved, error) {
	select {
	case r.builds <- struct{}{}:
	case <-ctx.Done():
		return Resolved{}, ctx.Err()
	}
	defer func() { <-r.builds }()
	return r.scratch(func(dir string) (Resolved, error) {
		// The out-link is a GC root that keeps the output alive until skopeo has read it.
		out, err := r.Tools.Build(ctx, installable, filepath.Join(dir, "result"))
		if err != nil {
			return Resolved{}, err
		}
		e, ok, err := r.Cache.Load(out)
		if err != nil {
			return Resolved{}, err
		}
		if !ok || !content.Has(r.Blobs, e.Blob) {
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
			if err := r.Cache.Save(out, e); err != nil {
				return Resolved{}, err
			}
		}
		return Resolved{Entry: e, Origin: &Origin{Installable: installable, Output: out}}, nil
	})
}

func (r *Resolver) scratch(fn func(dir string) (Resolved, error)) (Resolved, error) {
	dir, err := os.MkdirTemp(r.Scratch, ".image-")
	if err != nil {
		return Resolved{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	return fn(dir)
}
