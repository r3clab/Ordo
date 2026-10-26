package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const Egress = "egress"

const secretsFile = ".secrets.env"

// Podman's env-file reader rejects lines over 64 KiB, and Linux rejects argv strings over 128 KiB.
const MaxValueBytes = 32 << 10

const (
	EnvNamePattern     = `^[A-Za-z_][A-Za-z0-9_]*$`
	EnvValuePattern    = `^[^\x00]*$`
	SecretValuePattern = `^[^\x00\r\n]*$`
)

// NVIDIA's container hooks read these variables to inject devices outside the GPU allocation.
const ReservedEnvPrefix = "NVIDIA_"

var (
	envName     = regexp.MustCompile(EnvNamePattern)
	envValue    = regexp.MustCompile(EnvValuePattern)
	secretValue = regexp.MustCompile(SecretValuePattern)
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

func (s Sandbox) Validate(secrets Secrets) error {
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
	for name, value := range secrets {
		if err := checkVariable(name, value); err != nil {
			return err
		}
		// Podman's env-file format has no quoting.
		if !secretValue.MatchString(value) {
			return fmt.Errorf("secret %s contains a line break", name)
		}
		if _, ok := s.Env[name]; ok {
			return fmt.Errorf("variable %s is both env and a secret", name)
		}
	}
	return nil
}

func checkVariable(name, value string) error {
	if !envName.MatchString(name) {
		return fmt.Errorf("invalid environment variable name %q", name)
	}
	if strings.HasPrefix(name, ReservedEnvPrefix) {
		return fmt.Errorf("environment variable %s is reserved", name)
	}
	if len(value) > MaxValueBytes || !envValue.MatchString(value) {
		return fmt.Errorf("variable %s must be at most %d bytes without NUL", name, MaxValueBytes)
	}
	return nil
}

// Secrets holds launch-time values but serializes as its sorted names, so values never reach a record.
type Secrets map[string]string

func (s Secrets) Names() []string { return slices.Sorted(maps.Keys(s)) }

func (s Secrets) MarshalJSON() ([]byte, error) { return json.Marshal(s.Names()) }

func (s *Secrets) UnmarshalJSON(data []byte) error {
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return err
	}
	*s = Secrets{}
	for _, name := range names {
		(*s)[name] = ""
	}
	return nil
}

func writeSecrets(dir string, secrets Secrets) error {
	var text strings.Builder
	for _, name := range secrets.Names() {
		text.WriteString(name + "=" + secrets[name] + "\n")
	}
	return os.WriteFile(filepath.Join(dir, secretsFile), []byte(text.String()), 0o600)
}

func removeSecrets(dir string) error {
	if err := os.Remove(filepath.Join(dir, secretsFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s Sandbox) podmanArgs() []string {
	args := []string{"--network=none"}
	if s.Network == Egress {
		args = []string{"--network=pasta"}
	}
	for _, capability := range s.CapAdd {
		args = append(args, "--cap-add="+capability)
	}
	for _, name := range slices.Sorted(maps.Keys(s.Env)) {
		args = append(args, "--env", name+"="+s.Env[name])
	}
	return args
}
