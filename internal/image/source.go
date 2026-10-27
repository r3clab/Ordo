// Package image turns a task's image source into an immutable Podman image ID and its OCI archive blob.
package image

import (
	"errors"
	"regexp"
)

type Source struct {
	Nix string `json:"nix"`
}

const (
	MaxInstallableBytes = 4096
	InstallablePattern  = `^[^-\x00\r\n][^\x00\r\n]*$`
)

var installable = regexp.MustCompile(InstallablePattern)

func (s Source) Validate() error {
	if s.Nix == "" {
		return errors.New(`image must be {"nix": INSTALLABLE}`)
	}
	if len(s.Nix) > MaxInstallableBytes || !installable.MatchString(s.Nix) {
		return errors.New("invalid nix installable")
	}
	return nil
}
