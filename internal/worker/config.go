// Package worker drives rootless Podman on an SSH worker with shell scripts; nothing of Ordo is
// installed there.
package worker

import (
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
)

var stateDir = regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`)
var serviceHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`)

// DefaultStateDir is relative to the worker user's home.
const DefaultStateDir = ".local/state/ordo/worker"

// Unix socket paths stop at 108 bytes, and ssh's temporary bind names are longer than the final path.
const maxSocketPath = 100

type Config struct {
	Host      string   `json:"host"`
	StateDir  string   `json:"state_dir,omitempty"`
	CPUs      string   `json:"cpus"`
	MemoryMiB int64    `json:"memory_mib"`
	GPUs      []string `json:"gpus,omitempty"`
	Tags      []string `json:"tags,omitempty"`
}

func (c Config) Validate() error {
	if !spec.ValidGPUSet(c.GPUs) {
		return errors.New("GPUs must be unique full NVIDIA GPU UUIDs")
	}
	if !ssh.ValidHost(c.Host) {
		return errors.New("invalid SSH host alias")
	}
	if !stateDir.MatchString(c.StateDir) || !spec.CleanAbsolute(c.StateDir) || c.StateDir == "/" {
		return errors.New("remote state must be a clean absolute path without spaces or colons")
	}
	if _, err := spec.ParseCPUs(c.CPUs); err != nil {
		return err
	}
	if c.MemoryMiB <= 0 {
		return errors.New("positive remote memory budget required")
	}
	if !spec.ValidNameSet(c.Tags) {
		return errors.New("tags must be distinct names")
	}
	return nil
}

func (c Config) Carries(tags []string) bool {
	for _, tag := range tags {
		if !slices.Contains(c.Tags, tag) {
			return false
		}
	}
	return true
}

// cpus trusts that Validate accepted CPUs.
func (c Config) cpus() []int {
	cpus, _ := spec.ParseCPUs(c.CPUs)
	return cpus
}

func (c Config) Fits(r spec.Resources) bool {
	return len(c.cpus()) >= r.Cores && c.MemoryMiB >= r.MemoryMiB && len(c.GPUs) >= r.GPUs
}

func (c Config) runDir(attempt string) string { return path.Join(c.StateDir, "runs", attempt) }

func (c Config) treeDir(digest string) string { return path.Join(c.StateDir, "trees", digest) }

func (c Config) imageDir() string { return path.Join(c.StateDir, "images") }

// imageMark records an image that Ordo loaded. Prune removes only those, as an image the worker user
// pulled may carry the same ID.
func (c Config) imageMark(id string) string { return path.Join(c.imageDir(), id) }

func (c Config) serviceDir(name string) string { return path.Join(c.StateDir, "services", name) }

func (c Config) serviceSocket(name string) string { return path.Join(c.serviceDir(name), "socket") }

func (c Config) cdiDir() string { return path.Join(c.StateDir, "cdi") }

func (c Config) ServiceForward(name, address string) (string, error) {
	if !spec.ValidName(name) {
		return "", fmt.Errorf("invalid service name %q", name)
	}
	socket := c.serviceSocket(name)
	if len(socket) > maxSocketPath {
		return "", fmt.Errorf("service %s socket path is too long for a Unix socket", name)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("service %s: %w", name, err)
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 || (net.ParseIP(host) == nil && !serviceHost.MatchString(host)) {
		return "", fmt.Errorf("service %s needs a HOST:PORT address", name)
	}
	return socket + ":" + net.JoinHostPort(host, port), nil
}

// CheckLaunch measures t's launch script with placeholders as long as real values: SHA-256 image IDs
// and input digests, NVIDIA GPU UUIDs, and rand.Text() attempt and server IDs.
func (c Config) CheckLaunch(t spec.Task, inputs []string) error {
	digest := strings.Repeat("0", 64)
	s := spec.Spec{Image: "sha256:" + digest, Inputs: map[string]string{}, Task: t}
	for _, name := range inputs {
		s.Inputs[name] = digest
	}
	worst := pool.Allocation{CPUs: c.cpus(), MemoryMiB: t.Resources.MemoryMiB, GPUs: slices.Repeat([]string{"GPU-" + strings.Repeat("0", 36)}, t.Resources.GPUs)}
	placeholder := strings.Repeat("x", 26)
	secrets := map[string]string{}
	for _, name := range t.Secrets {
		secrets[name] = ""
	}
	script, _ := launchScript(c, Launch{Attempt: placeholder, Spec: s, Allocation: worst, Secrets: secrets}, placeholder)
	if err := ssh.CheckScript(script); err != nil {
		return fmt.Errorf("arguments and environment are too long for %s: %w", c.Host, err)
	}
	return nil
}
