// Package worker drives rootless Podman on an SSH worker with shell scripts; nothing of Ordo is
// installed there.
package worker

import (
	"errors"
	"fmt"
	"net"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
)

var stateDir = regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`)
var serviceHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`)

// Unix socket paths stop at 108 bytes, and ssh's temporary bind names are longer than the final path.
const maxSocketPath = 100

type Config struct {
	Host      string   `json:"host"`
	StateDir  string   `json:"state_dir"`
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
	if !stateDir.MatchString(c.StateDir) || path.Clean(c.StateDir) != c.StateDir || c.StateDir == "/" {
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

func (c Config) runDir(attempt string) string { return path.Join(c.StateDir, "runs", attempt) }

func (c Config) treeDir(digest string) string { return path.Join(c.StateDir, "trees", digest) }

func (c Config) serviceDir(name string) string { return path.Join(c.StateDir, "services", name) }

func (c Config) cdiDir() string { return path.Join(c.StateDir, "cdi") }

func (c Config) ServiceForward(name, address string) (string, error) {
	if !spec.ValidName(name) {
		return "", fmt.Errorf("invalid service name %q", name)
	}
	socket := path.Join(c.serviceDir(name), "socket")
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

func (c Config) CheckLaunch(s spec.Spec, secrets map[string]string) error {
	cpus, err := spec.ParseCPUs(c.CPUs)
	if err != nil {
		return err
	}
	worst := pool.Allocation{CPUs: cpus, MemoryMiB: s.Resources.MemoryMiB}
	for range s.Resources.GPUs {
		worst.GPUs = append(worst.GPUs, "GPU-"+strings.Repeat("0", 36))
	}
	// The placeholders are as long as real values: NVIDIA GPU UUIDs, and rand.Text() attempt and server IDs.
	placeholder := strings.Repeat("x", 26)
	script, _ := launchScript(c, Launch{Attempt: placeholder, Spec: s, Allocation: worst, Secrets: secrets}, placeholder)
	if err := ssh.CheckScript(script); err != nil {
		return fmt.Errorf("arguments and environment are too long for %s: %w", c.Host, err)
	}
	return nil
}
