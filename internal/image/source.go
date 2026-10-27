// Package image turns a task's image source into an immutable Podman image ID and its OCI archive blob.
package image

import (
	"errors"
	"regexp"
	"strings"
)

// Source is a nix installable, or a registry image as docker://NAME@sha256:DIGEST.
type Source string

const (
	MaxInstallableBytes = 4096
	InstallablePattern  = `^[^-\x00\r\n][^\x00\r\n]*$`
	registryPrefix      = "docker://"
)

var (
	installable = regexp.MustCompile(InstallablePattern)
	// The digest pins the image, so a pull yields what the job was submitted with.
	reference = regexp.MustCompile(`^docker://[^\s@]+@sha256:[0-9a-f]{64}$`)
)

// Registry reports whether s names a registry image rather than a nix installable.
func (s Source) Registry() bool { return strings.HasPrefix(string(s), registryPrefix) }

func (s Source) Validate() error {
	switch {
	case s == "":
		return errors.New("image must be a nix installable or a docker:// reference")
	case len(s) > MaxInstallableBytes:
		return errors.New("image source is too long")
	case s.Registry() && !reference.MatchString(string(s)):
		return errors.New("a docker:// image must be pinned as NAME@sha256:DIGEST")
	case !installable.MatchString(string(s)):
		return errors.New("invalid nix installable")
	}
	return nil
}
