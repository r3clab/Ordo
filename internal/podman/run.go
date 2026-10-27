// Package podman builds the Podman commands Ordo runs on workers and parses their output.
package podman

import (
	"maps"
	"slices"
	"strconv"

	"github.com/r3clab/Ordo/internal/spec"
)

const (
	AttemptLabel = "io.r3clab.ordo.attempt"
	ServerLabel  = "io.r3clab.ordo.server"
)

// Name derives the container name from the attempt, so a repeated launch cannot start a second container.
func Name(attempt string) string { return "ordo-" + attempt }

type Run struct {
	Attempt   string
	Server    string
	Image     string
	Args      []string
	CPUs      []int
	MemoryMiB int64
	spec.Sandbox
	Inputs     map[string]string
	Services   map[string]string
	Outputs    string
	GPUs       []string
	CDISpecDir string
	EnvFile    string
	Timeout    int64
}

// Argv follows `podman --remote=false`; CDI's spec directory is a global flag, so it precedes `run`.
func (r Run) Argv() []string {
	memory := strconv.FormatInt(r.MemoryMiB*1024*1024, 10)
	var args []string
	if len(r.GPUs) > 0 {
		args = append(args, "--cdi-spec-dir", r.CDISpecDir)
	}
	args = append(args,
		"run", "--detach", "--pull=never", "--name", Name(r.Attempt),
		"--label", AttemptLabel+"="+r.Attempt, "--label", ServerLabel+"="+r.Server,
		"--cpuset-cpus", spec.FormatCPUs(r.CPUs), "--memory", memory, "--memory-swap", memory,
	)
	if r.Timeout > 0 {
		args = append(args, "--timeout", strconv.FormatInt(r.Timeout, 10))
	}
	if r.Network == spec.Egress {
		args = append(args, "--network=pasta")
	} else {
		args = append(args, "--network=none")
	}
	for _, capability := range r.CapAdd {
		args = append(args, "--cap-add="+capability)
	}
	for _, name := range slices.Sorted(maps.Keys(r.Env)) {
		args = append(args, "--env", name+"="+r.Env[name])
	}
	// Image VOLUMEs would add writable storage outside /outputs that outlives the container, and
	// k8s-file logs stay readable by podman logs whatever the host's journald keeps.
	args = append(args, "--image-volume=ignore", "--log-driver=k8s-file")
	for _, name := range slices.Sorted(maps.Keys(r.Inputs)) {
		args = append(args, "--volume", r.Inputs[name]+":/inputs/"+name+":ro")
	}
	// A directory mount, unlike a socket file mount, sees the socket recreated by a reconnected SSH forward.
	for _, name := range slices.Sorted(maps.Keys(r.Services)) {
		args = append(args, "--volume", r.Services[name]+":/services/"+name+":ro")
	}
	args = append(args, "--volume", r.Outputs+":/outputs:rw")
	for _, id := range r.GPUs {
		args = append(args, "--device", "nvidia.com/gpu="+id)
	}
	if len(r.GPUs) > 0 {
		args = append(args, "--env", "NVIDIA_VISIBLE_DEVICES=void")
	}
	if r.EnvFile != "" {
		args = append(args, "--env-file", r.EnvFile)
	}
	args = append(args, "--", r.Image)
	return append(args, r.Args...)
}
