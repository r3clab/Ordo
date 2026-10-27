package spec

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const Egress = "egress"

// Podman's env-file reader rejects lines over 64 KiB, and Linux rejects argv strings over 128 KiB.
const MaxValueBytes = 32 << 10

const (
	EnvNamePattern  = `^[A-Za-z_][A-Za-z0-9_]*$`
	EnvValuePattern = `^[^\x00]*$`
)

// NVIDIA's container hooks read these variables to inject devices outside the GPU allocation.
const ReservedEnvPrefix = "NVIDIA_"

var (
	envName  = regexp.MustCompile(EnvNamePattern)
	envValue = regexp.MustCompile(EnvValuePattern)
)

var networks = []string{"none", Egress}

// Each allowed capability acts only on namespaces owned by the rootless user namespace.
var capabilities = []string{"NET_ADMIN"}

func Networks() []string     { return slices.Clone(networks) }
func Capabilities() []string { return slices.Clone(capabilities) }

type Sandbox struct {
	Env     map[string]string `json:"env,omitempty"`
	Network string            `json:"network,omitempty"`
	CapAdd  []string          `json:"cap_add,omitempty"`
}

func (s Sandbox) Validate() error {
	if s.Network != "" && !slices.Contains(networks, s.Network) {
		return errors.New(`network must be "none" or "egress"`)
	}
	for i, capability := range s.CapAdd {
		if !slices.Contains(capabilities, capability) {
			return fmt.Errorf("capability %q is not allowed; allowed: %s", capability, strings.Join(capabilities, ", "))
		}
		if slices.Contains(s.CapAdd[:i], capability) {
			return fmt.Errorf("capability %s is repeated", capability)
		}
	}
	for name, value := range s.Env {
		if err := checkVariable(name, value); err != nil {
			return err
		}
	}
	return nil
}

func CheckSecret(name, value string) error {
	if err := checkVariable(name, value); err != nil {
		return err
	}
	if value == "" {
		return fmt.Errorf("secret %s is empty", name)
	}
	// Podman's env-file format has no quoting.
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("secret %s contains a line break", name)
	}
	return nil
}

func checkName(name string) error {
	if !envName.MatchString(name) {
		return fmt.Errorf("invalid environment variable name %q", name)
	}
	if strings.HasPrefix(name, ReservedEnvPrefix) {
		return fmt.Errorf("environment variable %s is reserved", name)
	}
	return nil
}

func checkVariable(name, value string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if len(value) > MaxValueBytes || !envValue.MatchString(value) {
		return fmt.Errorf("variable %s must be at most %d bytes without NUL", name, MaxValueBytes)
	}
	return nil
}
