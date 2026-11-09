// Package image turns a task's image source into an immutable Podman image ID and its OCI archive blob.
package image

import (
	"errors"
	"regexp"
	"strings"
)

// A Source resolves once, when its job is submitted: an installable to the store path it builds, a
// registry image to the manifest digest its tag names unless it gives one.
type Source string

const (
	MaxInstallableBytes = 4096
	InstallablePattern  = `^[^-\x00\r\n][^\x00\r\n]*$`
	registryPrefix      = "docker://"
)

var (
	installable = regexp.MustCompile(InstallablePattern)
	reference   = regexp.MustCompile(`^docker://([^\s@]+)(?:@(sha256:[0-9a-f]{64}))?$`)
)

// Registry reports whether s names a registry image rather than a nix installable.
func (s Source) Registry() bool { return strings.HasPrefix(string(s), registryPrefix) }

// A tag beside a digest only labels it, as skopeo refuses a reference that carries both.
func (s Source) pin() (repository, digest string, ok bool) {
	m := reference.FindStringSubmatch(string(s))
	if m == nil {
		return "", "", false
	}
	repository = m[1]
	if i := strings.LastIndexByte(repository, ':'); i > strings.LastIndexByte(repository, '/') {
		repository = repository[:i]
	}
	return repository, m[2], repository != ""
}

func (s Source) Validate() error {
	switch {
	case s == "":
		return errors.New("image must be a nix installable or a docker:// reference")
	case len(s) > MaxInstallableBytes:
		return errors.New("image source is too long")
	case s.Registry():
		if _, _, ok := s.pin(); !ok {
			return errors.New("a docker:// image must be NAME[:TAG][@sha256:DIGEST]")
		}
	case !installable.MatchString(string(s)):
		return errors.New("invalid nix installable")
	}
	return nil
}
