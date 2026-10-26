// Package imagetest fakes the nix and skopeo commands behind image.Tools.
package imagetest

import (
	"archive/tar"
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

var manifest = strings.Repeat("a", 64)

func Manifest(config string) string { return `{"config":{"digest":"` + config + `"}}` }

func Layout(manifestJSON string) map[string]string {
	return map[string]string{
		"oci-layout":               `{"imageLayoutVersion":"1.0.0"}`,
		"index.json":               `{"manifests":[{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + manifest + `"}]}`,
		"blobs/sha256/" + manifest: manifestJSON,
	}
}

func WriteArchive(file string, files map[string]string) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(f)
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(files[name])), Typeflag: tar.TypeReg}); err != nil {
			return errors.Join(err, f.Close())
		}
		if _, err := tw.Write([]byte(files[name])); err != nil {
			return errors.Join(err, f.Close())
		}
	}
	return errors.Join(tw.Close(), f.Close())
}

type Nix struct {
	Output  string
	Config  string
	Release chan struct{}

	Builds, Exports atomic.Int32

	mu   sync.Mutex
	argv map[string][]string
}

func (n *Nix) Argv(name string) []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.argv[name]
}

func (n *Nix) Exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	n.mu.Lock()
	if n.argv == nil {
		n.argv = map[string][]string{}
	}
	n.argv[name] = args
	n.mu.Unlock()
	switch name {
	case "nix":
		n.Builds.Add(1)
		if n.Release != nil {
			select {
			case <-n.Release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []byte(n.Output + "\n"), nil
	case "skopeo":
		n.Exports.Add(1)
		dest, ok := strings.CutPrefix(args[len(args)-1], "oci-archive:")
		if !ok {
			return nil, errors.New("skopeo must copy to an oci-archive")
		}
		return nil, WriteArchive(dest, Layout(Manifest(n.Config)))
	}
	return nil, errors.New("unexpected command " + name)
}
