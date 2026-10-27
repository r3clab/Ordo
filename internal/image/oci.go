package image

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/r3clab/Ordo/internal/spec"
)

const maxMetadataBytes = 1 << 20

// Podman uses an image's config digest as its ID.
func ArchiveID(archive string) (string, error) {
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var index struct {
		Manifests []struct {
			MediaType string `json:"mediaType"`
			Digest    string `json:"digest"`
		} `json:"manifests"`
	}
	if err := readJSON(f, "index.json", &index); err != nil {
		return "", err
	}
	if len(index.Manifests) != 1 || index.Manifests[0].MediaType != "application/vnd.oci.image.manifest.v1+json" {
		return "", errors.New("OCI archive must hold exactly one image manifest")
	}
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := readJSON(f, blobPath(index.Manifests[0].Digest), &manifest); err != nil {
		return "", err
	}
	if !spec.ValidImageID(manifest.Config.Digest) {
		return "", errors.New("OCI manifest has an invalid config digest")
	}
	return manifest.Config.Digest, nil
}

func blobPath(digest string) string { return "blobs/" + strings.Replace(digest, ":", "/", 1) }

func readJSON(f *os.File, name string, value any) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("OCI archive lacks %s", name)
		}
		if err != nil {
			return err
		}
		if path.Clean(h.Name) != name || h.Typeflag != tar.TypeReg {
			continue
		}
		if h.Size > maxMetadataBytes {
			return fmt.Errorf("OCI archive %s is too large", name)
		}
		return json.NewDecoder(tr).Decode(value)
	}
}
