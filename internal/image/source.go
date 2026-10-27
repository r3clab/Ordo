// Package image turns a task's image source into an immutable Podman image ID and its OCI archive blob.
package image

import (
	"errors"
	"regexp"
)

type Source string

const (
	MaxInstallableBytes = 4096
	InstallablePattern  = `^[^-\x00\r\n][^\x00\r\n]*$`
)

var installable = regexp.MustCompile(InstallablePattern)

func (s Source) Validate() error {
	if s == "" {
		return errors.New("image must be a nix installable")
	}
	if len(s) > MaxInstallableBytes || !installable.MatchString(string(s)) {
		return errors.New("invalid nix installable")
	}
	return nil
}
