// Package image turns a task's image source into an immutable Podman image ID and its OCI archive blob.
package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"

	"github.com/r3clab/Ordo/internal/task"
)

// Source is exactly one of a local Podman reference, an OCI archive with the image ID inside it, or a
// nix installable the server builds.
type Source struct {
	Ref     string
	Archive string
	ID      string
	Nix     string
}

const (
	MaxInstallableBytes = 4096
	InstallablePattern  = `^[^-\x00\r\n][^\x00\r\n]*$`
)

var installable = regexp.MustCompile(InstallablePattern)

var errShape = errors.New(`image must be a reference, {"nix": INSTALLABLE} or {"archive": PATH, "id": IMAGE_ID}`)

type object struct {
	Nix     string `json:"nix,omitempty"`
	Archive string `json:"archive,omitempty"`
	ID      string `json:"id,omitempty"`
}

func (s Source) IsZero() bool { return s == Source{} }

func (s Source) IsNix() bool { return s.Nix != "" }

func (s Source) Validate() error {
	switch s {
	case Source{Ref: s.Ref}:
		if s.Ref == "" {
			return errShape
		}
	case Source{Nix: s.Nix}:
		if len(s.Nix) > MaxInstallableBytes || !installable.MatchString(s.Nix) {
			return errors.New("invalid nix installable")
		}
	case Source{Archive: s.Archive, ID: s.ID}:
		if s.Archive == "" || !task.ValidImageID(s.ID) {
			return errors.New("an OCI archive needs its path and immutable sha256 image ID")
		}
	default:
		return errShape
	}
	return nil
}

func (s Source) MarshalJSON() ([]byte, error) {
	if s.Ref != "" {
		return json.Marshal(s.Ref)
	}
	return json.Marshal(object{Nix: s.Nix, Archive: s.Archive, ID: s.ID})
}

func (s *Source) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	switch {
	case bytes.Equal(data, []byte("null")):
		return nil
	case bytes.HasPrefix(data, []byte(`"`)):
		*s = Source{}
		return json.Unmarshal(data, &s.Ref)
	case bytes.HasPrefix(data, []byte("{")):
		var v object
		// The caller's DisallowUnknownFields does not reach a custom unmarshaler.
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(&v); err != nil {
			return err
		}
		if v == (object{}) {
			return errShape
		}
		*s = Source{Nix: v.Nix, Archive: v.Archive, ID: v.ID}
		return nil
	}
	return errShape
}
