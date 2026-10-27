// Package spec defines what a job asks a worker to run and validates it.
package spec

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"time"
)

const NamePattern = `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`

var identifier = regexp.MustCompile(NamePattern)

var imageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const MaxTimeoutSeconds = 365 * 24 * 60 * 60

func ValidName(name string) bool { return identifier.MatchString(name) }

func ValidNameSet(names []string) bool {
	_, dup := repeated(names)
	return !dup && !slices.ContainsFunc(names, func(name string) bool { return !ValidName(name) })
}

func repeated[T comparable](values []T) (T, bool) {
	for i, v := range values {
		if slices.Contains(values[:i], v) {
			return v, true
		}
	}
	var zero T
	return zero, false
}

func ValidImageID(id string) bool { return imageID.MatchString(id) }

type Resources struct {
	Cores     int   `json:"cores"`
	MemoryMiB int64 `json:"memory_mib"`
	GPUs      int   `json:"gpus,omitempty"`
}

type Spec struct {
	Image     string            `json:"image"`
	Args      []string          `json:"args,omitempty"`
	Resources Resources         `json:"resources"`
	Inputs    map[string]string `json:"inputs,omitempty"`
	Sandbox
	Secrets        []string `json:"secrets,omitempty"`
	Services       []string `json:"services,omitempty"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty"`
	Tags           []string `json:"tags,omitempty"`
}

func (s Spec) Normalize() Spec {
	s.Args = slices.Clone(s.Args)
	s.Inputs = maps.Clone(s.Inputs)
	s.Env = maps.Clone(s.Env)
	s.CapAdd = slices.Sorted(slices.Values(s.CapAdd))
	s.Secrets = slices.Sorted(slices.Values(s.Secrets))
	s.Services = slices.Sorted(slices.Values(s.Services))
	s.Tags = slices.Sorted(slices.Values(s.Tags))
	return s
}

func (s Spec) Validate() error {
	if s.Resources.Cores <= 0 || s.Resources.MemoryMiB <= 0 || s.Resources.MemoryMiB > math.MaxInt64>>20 || s.Resources.GPUs < 0 {
		return errors.New("positive cores/memory and a non-negative GPU count are required")
	}
	if s.TimeoutSeconds < 0 || s.TimeoutSeconds > MaxTimeoutSeconds {
		return fmt.Errorf("timeout must be 0..%d seconds", MaxTimeoutSeconds)
	}
	if err := s.Sandbox.Validate(); err != nil {
		return err
	}
	if name, dup := repeated(s.Secrets); dup {
		return fmt.Errorf("secret %s is repeated", name)
	}
	for _, name := range s.Secrets {
		if err := checkName(name); err != nil {
			return err
		}
		if _, ok := s.Env[name]; ok {
			return fmt.Errorf("variable %s is both env and a secret", name)
		}
	}
	if !ValidNameSet(s.Services) {
		return errors.New("invalid or duplicate service name")
	}
	if !ValidNameSet(s.Tags) {
		return errors.New("invalid or duplicate tag")
	}
	return nil
}

func (s Spec) Timeout() time.Duration { return time.Duration(s.TimeoutSeconds) * time.Second }

func (s Spec) TimedOut(start, end time.Time) bool {
	timeout := s.Timeout()
	return timeout > 0 && end.Sub(start) >= timeout
}
