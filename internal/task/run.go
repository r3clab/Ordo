package task

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/content"
)

type Request struct {
	Image         string            `json:"image"`
	CPUs          string            `json:"cpus"`
	MemoryMiB     int64             `json:"memory_mib"`
	Args          []string          `json:"args,omitempty"`
	OutputDir     string            `json:"output_dir"`
	ReservationID string            `json:"reservation_id,omitempty"`
	Inputs        map[string]string `json:"inputs,omitempty"`
	ExportOutputs bool              `json:"export_outputs,omitempty"`
	GPUs          []string          `json:"gpus,omitempty"`
	CDISpecDir    string            `json:"cdi_spec_dir,omitempty"`
	Sandbox
	Secrets  Secrets           `json:"secrets,omitempty"`
	Services map[string]string `json:"services,omitempty"`
}

type ExitError struct {
	Code      int
	OutputDir string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("task exited with status %d; results: %s", e.Code, e.OutputDir)
}

type Record struct {
	Request          Request   `json:"request"`
	ContainerName    string    `json:"container_name"`
	ContainerID      string    `json:"container_id,omitempty"`
	ImageID          string    `json:"image_id,omitempty"`
	State            State     `json:"state"`
	CreatedAt        time.Time `json:"created_at"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	ExitCode         *int      `json:"exit_code,omitempty"`
	OOMKilled        bool      `json:"oom_killed"`
	ContainerRemoved bool      `json:"container_removed"`
	Error            string    `json:"error,omitempty"`
	AllocationMode   string    `json:"allocation_mode,omitempty"`
	OutputsSHA256    string    `json:"outputs_sha256,omitempty"`
}

type command func(context.Context, io.Writer, io.Writer, ...string) error

func Run(ctx context.Context, req Request) error {
	return run(ctx, req, execPodman)
}

func run(ctx context.Context, req Request, podman command) error {
	if err := req.validate(); err != nil {
		return err
	}
	outputDir, err := filepath.Abs(req.OutputDir)
	if err != nil {
		return err
	}
	req.OutputDir = outputDir
	if err := os.Mkdir(outputDir, 0o700); err != nil {
		return fmt.Errorf("create output directory (must not exist): %w", err)
	}
	lock, err := lockRun(outputDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	r := Record{
		Request:        req,
		ContainerName:  "ordo-" + strings.ToLower(rand.Text()),
		State:          Preparing,
		CreatedAt:      time.Now().UTC(),
		AllocationMode: "unmanaged",
	}
	if req.ReservationID != "" {
		r.AllocationMode = "agent"
	}
	if err := saveRecord(&r); err != nil {
		return err
	}
	return outcome(&r, execute(ctx, &r, podman))
}

func outcome(r *Record, err error) error {
	if err != nil {
		// Finished is the durable results marker, even if subsequent cleanup fails.
		if r.State != Finished {
			r.State = Errored
			if errors.Is(err, context.Canceled) {
				r.State = Interrupted
			}
		}
		r.Error = err.Error()
		return fmt.Errorf("run %s (container %s): %w", r.Request.OutputDir, r.ContainerName, errors.Join(err, saveRecord(r)))
	}
	if *r.ExitCode != 0 {
		return &ExitError{Code: *r.ExitCode, OutputDir: r.Request.OutputDir}
	}
	return nil
}

func (r Request) validate() error {
	if len(r.GPUs) > 0 {
		if !filepath.IsAbs(r.CDISpecDir) {
			return errors.New("GPU requests require a private CDI specification directory")
		}
		if _, err := CheckGPUs(r.GPUs); err != nil {
			return err
		}
	}
	if err := checkMounts("input", r.Inputs); err != nil {
		return err
	}
	if err := checkMounts("service", r.Services); err != nil {
		return err
	}
	if err := r.Validate(r.Secrets); err != nil {
		return err
	}
	if strings.TrimSpace(r.Image) == "" {
		return errors.New("--image is required (load a nix2container image into Podman first)")
	}
	if r.OutputDir == "" {
		return errors.New("--output is required")
	}
	if r.MemoryMiB <= 0 || r.MemoryMiB > math.MaxInt64/(1024*1024) {
		return errors.New("--memory-mib must be a positive, representable memory budget")
	}
	if _, err := ParseCPUs(r.CPUs); err != nil {
		return fmt.Errorf("--cpus: %w", err)
	}
	return nil
}

func checkMounts(kind string, mounts map[string]string) error {
	for name, dir := range mounts {
		if name == "" || strings.ContainsAny(name, "/\\:\x00\n") || name == "." || name == ".." || !filepath.IsAbs(dir) || strings.ContainsAny(dir, ":\x00\n") {
			return fmt.Errorf("invalid %s mount", kind)
		}
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s mount must be a directory", kind)
		}
	}
	return nil
}

func execute(ctx context.Context, r *Record, podman command) error {
	info, err := podman.output(ctx, "info", "--format=json")
	if err != nil {
		return err
	}
	if err := checkHost(info); err != nil {
		return err
	}

	imageID, err := podman.imageID(ctx, r.Request.Image)
	if err != nil {
		return fmt.Errorf("inspect local image; load it with nix2container's copyToPodman first: %w", err)
	}
	r.ImageID = imageID
	if err := saveRecord(r); err != nil {
		return err
	}

	memory := strconv.FormatInt(r.Request.MemoryMiB*1024*1024, 10)
	// Reservations belong to the agent; this layer enforces the selected CPU and memory limits.
	args := []string{
		"run", "--detach", "--pull=never", "--name", r.ContainerName,
		"--label", runLabel + "=" + r.ContainerName,
		"--cpuset-cpus", r.Request.CPUs, "--memory", memory, "--memory-swap", memory,
	}
	args = append(args, r.Request.podmanArgs()...)
	args = append(args, "--image-volume=ignore", "--log-driver=k8s-file")
	for _, name := range slices.Sorted(maps.Keys(r.Request.Inputs)) {
		args = append(args, "--volume", r.Request.Inputs[name]+":/inputs/"+name+":ro")
	}
	// A directory mount, unlike a socket file mount, sees the socket recreated by a reconnecting tunnel.
	for _, name := range slices.Sorted(maps.Keys(r.Request.Services)) {
		args = append(args, "--volume", r.Request.Services[name]+":/services/"+name+":ro")
	}
	if r.Request.ExportOutputs {
		outputs := filepath.Join(r.Request.OutputDir, OutputsDir)
		if err := os.Mkdir(outputs, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(outputs, 0o777); err != nil {
			return err
		}
		args = append(args, "--volume", outputs+":/outputs:rw")
	}
	if len(r.Request.GPUs) > 0 {
		args = append([]string{"--cdi-spec-dir", r.Request.CDISpecDir}, args...)
		for _, id := range r.Request.GPUs {
			args = append(args, "--device", "nvidia.com/gpu="+id)
		}
		args = append(args, "--env", "NVIDIA_VISIBLE_DEVICES=void")
	}
	if len(r.Request.Secrets) > 0 {
		if err := writeSecrets(r.Request.OutputDir, r.Request.Secrets); err != nil {
			return errors.Join(err, removeSecrets(r.Request.OutputDir))
		}
		args = append(args, "--env-file", filepath.Join(r.Request.OutputDir, secretsFile))
	}
	args = append(args, "--", r.ImageID)
	args = append(args, r.Request.Args...)
	containerID, err := podman.output(ctx, args...)
	if err := errors.Join(err, removeSecrets(r.Request.OutputDir)); err != nil {
		return err
	}
	id := strings.TrimSpace(string(containerID))
	if !content.Valid(id) {
		return errors.New("container creation did not return a valid ID; retain the run directory for recovery")
	}
	r.ContainerID = id
	r.State = Running
	if err := saveRecord(r); err != nil {
		return err
	}

	return complete(ctx, r, podman)
}

func complete(ctx context.Context, r *Record, podman command) error {
	if _, err := podman.output(ctx, "wait", "--", r.ContainerID); err != nil {
		return err
	}
	container, err := podman.inspectOwned(ctx, r, r.ContainerID)
	if err != nil {
		return err
	}
	if container.State.Status != "exited" {
		return errors.New("container has not exited; retaining it for inspection")
	}
	state := container.State
	if state.ExitCode == nil || *state.ExitCode < 0 || *state.ExitCode > 255 {
		return errors.New("container inspection did not return a valid exit code")
	}
	r.ExitCode = state.ExitCode
	r.StartedAt = state.StartedAt
	r.FinishedAt = state.FinishedAt
	r.OOMKilled = state.OOMKilled
	if err := collectLogs(ctx, r, podman); err != nil {
		return err
	}
	if r.Request.ExportOutputs {
		hash, err := content.Archive(filepath.Join(r.Request.OutputDir, OutputsDir), filepath.Join(r.Request.OutputDir, OutputsArchive))
		if err != nil {
			return fmt.Errorf("collect outputs: %w", err)
		}
		r.OutputsSHA256 = hash
	}
	r.State = Finished
	r.Error = ""
	if err := saveRecord(r); err != nil {
		return err
	}
	return cleanup(ctx, r, podman)
}

func cleanup(ctx context.Context, r *Record, podman command) error {
	// Keep the container unless its logs and final metadata have reached disk.
	if _, err := podman.output(ctx, "rm", "--", r.ContainerID); err != nil {
		return err
	}
	r.ContainerRemoved = true
	r.Error = ""
	return saveRecord(r)
}

type podmanHost struct {
	OS                string
	ServiceIsRemote   bool
	CgroupVersion     string
	CgroupControllers []string
	Security          struct{ Rootless bool }
}

func readLocalHost(data []byte) (podmanHost, error) {
	var info struct{ Host podmanHost }
	if err := json.Unmarshal(data, &info); err != nil {
		return podmanHost{}, fmt.Errorf("decode Podman info: %w", err)
	}
	if info.Host.OS != "linux" || info.Host.ServiceIsRemote || !info.Host.Security.Rootless {
		return podmanHost{}, errors.New("execution requires local, rootless Podman on Linux")
	}
	return info.Host, nil
}

func checkHost(data []byte) error {
	host, err := readLocalHost(data)
	if err != nil {
		return err
	}
	if host.CgroupVersion != "v2" {
		return errors.New("execution requires cgroup v2 for rootless resource limits")
	}
	for _, controller := range []string{"cpuset", "memory"} {
		if !slices.Contains(host.CgroupControllers, controller) {
			return fmt.Errorf("missing delegated %s cgroup controller; configure delegation for the worker's user session", controller)
		}
	}
	return nil
}

func execPodman(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "podman", append([]string{"--remote=false"}, args...)...)
	if os.Getenv("INVOCATION_ID") != "" {
		// FUSE mounts and other runtime helpers must outlive the agent's service cgroup.
		flags := []string{"--user", "--scope", "--quiet", "--collect", "--property=Delegate=yes", "--unit=ordo-runtime-" + strings.ToLower(rand.Text())}
		optOut, err := scopeExpansionOptOut(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("probe runtime scope support: %w", err)
		}
		if optOut {
			flags = append(flags, "--expand-environment=no")
		}
		cmd = exec.CommandContext(ctx, "systemd-run", append(append(flags, "--"), cmd.Args...)...)
	}
	// Do not propagate agent ownership; systemd-run supplies its new scope's identity.
	cmd.Env = slices.DeleteFunc(os.Environ(), func(value string) bool { return strings.HasPrefix(value, "INVOCATION_ID=") })
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

var scopeProbe struct {
	sync.Mutex
	done, optOut bool
}

func scopeExpansionOptOut(ctx context.Context) (bool, error) {
	scopeProbe.Lock()
	defer scopeProbe.Unlock()
	if !scopeProbe.done {
		help, err := exec.CommandContext(ctx, "systemd-run", "--help").Output()
		if err != nil {
			return false, err
		}
		// Old systemd scopes never expand argv; newer versions provide an explicit opt-out.
		scopeProbe.optOut = bytes.Contains(help, []byte("--expand-environment="))
		scopeProbe.done = true
	}
	return scopeProbe.optOut, nil
}

func (podman command) output(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	if err := podman(ctx, &stdout, &stderr, args...); err != nil {
		return nil, fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func collectLogs(ctx context.Context, r *Record, podman command) error {
	dir := r.Request.OutputDir
	for _, name := range logFiles {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && !info.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular %s", name)
		}
	}
	return content.WriteAtomic(filepath.Join(dir, StdoutFile), func(stdout io.Writer) error {
		return content.WriteAtomic(filepath.Join(dir, StderrFile), func(stderr io.Writer) error {
			return podman(ctx, stdout, stderr, "logs", "--", r.ContainerID)
		})
	})
}

func saveRecord(r *Record) error {
	return content.WriteAtomic(filepath.Join(r.Request.OutputDir, RecordFile), func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(r)
	})
}
