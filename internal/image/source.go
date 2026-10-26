// Package image turns a task's image source into an immutable Podman image ID and its OCI archive blob.
package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
)

type Source struct {
	Ref string
	Nix string
}

const (
	MaxInstallableBytes = 4096
	InstallablePattern  = `^[^-\x00\r\n][^\x00\r\n]*$`
)

var installable = regexp.MustCompile(InstallablePattern)

type nixObject struct {
	Nix string `json:"nix"`
}

func (s Source) IsZero() bool { return s == Source{} }

func (s Source) IsNix() bool { return s.Nix != "" }

func (s Source) Validate(archive string) error {
	switch {
	case (s.Ref == "") == !s.IsNix():
		return errors.New(`image must be a reference or {"nix": INSTALLABLE}`)
	case s.IsNix() && archive != "":
		return errors.New("archive applies only to a local image reference")
	case s.IsNix() && (len(s.Nix) > MaxInstallableBytes || !installable.MatchString(s.Nix)):
		return errors.New("invalid nix installable")
	}
	return nil
}

func (s Source) MarshalJSON() ([]byte, error) {
	if s.IsNix() {
		return json.Marshal(nixObject{s.Nix})
	}
	return json.Marshal(s.Ref)
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
		var v nixObject
		// The caller's DisallowUnknownFields does not reach a custom unmarshaler.
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(&v); err != nil {
			return err
		}
		if v.Nix == "" {
			return errors.New("image nix installable is empty")
		}
		*s = Source{Nix: v.Nix}
		return nil
	}
	return errors.New(`image must be a string or {"nix": INSTALLABLE}`)
}
