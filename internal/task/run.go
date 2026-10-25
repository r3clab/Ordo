package task

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Request struct {
	Image     string   `json:"image"`
	CPUs      string   `json:"cpus"`
	MemoryMiB int64    `json:"memory_mib"`
	Args      []string `json:"args,omitempty"`
	OutputDir string   `json:"output_dir"`
}

type ExitError struct {
	Code      int
	OutputDir string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("task exited with status %d; results: %s", e.Code, e.OutputDir)
}

type record struct {
	Request          Request   `json:"request"`
	ContainerName    string    `json:"container_name"`
	ContainerID      string    `json:"container_id,omitempty"`
	ImageID          string    `json:"image_id,omitempty"`
	State            string    `json:"state"`
	CreatedAt        time.Time `json:"created_at"`
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	ExitCode         *int      `json:"exit_code,omitempty"`
	OOMKilled        bool      `json:"oom_killed"`
	ContainerRemoved bool      `json:"container_removed"`
	Error            string    `json:"error,omitempty"`
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
	r := record{
		Request:       req,
		ContainerName: "ordo-" + strings.ToLower(rand.Text()),
		State:         "preparing",
		CreatedAt:     time.Now().UTC(),
	}
	if err := saveRecord(&r); err != nil {
		return err
	}
	if err := execute(ctx, &r, podman); err != nil {
		r.State = "error"
		if errors.Is(err, context.Canceled) {
			r.State = "interrupted"
		}
		r.Error = err.Error()
		return fmt.Errorf("run %s (container %s): %w", outputDir, r.ContainerName, errors.Join(err, saveRecord(&r)))
	}
	if *r.ExitCode != 0 {
		return &ExitError{Code: *r.ExitCode, OutputDir: outputDir}
	}
	return nil
}

func (r Request) validate() error {
	if strings.TrimSpace(r.Image) == "" {
		return errors.New("--image is required (load a nix2container image into Podman first)")
	}
	if r.OutputDir == "" {
		return errors.New("--output is required")
	}
	if r.MemoryMiB <= 0 || r.MemoryMiB > math.MaxInt64/(1024*1024) {
		return errors.New("--memory-mib must be a positive, representable memory budget")
	}
	for _, part := range strings.Split(r.CPUs, ",") {
		first, last, isRange := strings.Cut(part, "-")
		start, err := strconv.ParseUint(first, 10, 32)
		if err != nil {
			return errors.New("--cpus requires CPU IDs or ranges, such as 0,2-3")
		}
		if isRange {
			end, err := strconv.ParseUint(last, 10, 32)
			if err != nil || start > end {
				return fmt.Errorf("invalid CPU range %q", part)
			}
		}
	}
	return nil
}

func execute(ctx context.Context, r *record, podman command) error {
	info, err := podman.output(ctx, "info", "--format=json")
	if err != nil {
		return err
	}
	if err := checkHost(info); err != nil {
		return err
	}

	image, err := podman.output(ctx, "image", "inspect", "--format=json", "--", r.Request.Image)
	if err != nil {
		return fmt.Errorf("inspect local image; load it with nix2container's copyToPodman first: %w", err)
	}
	var images []struct{ ID string }
	if err := json.Unmarshal(image, &images); err != nil {
		return fmt.Errorf("decode image inspection: %w", err)
	}
	if len(images) != 1 {
		return errors.New("expected exactly one local image")
	}
	imageID := strings.TrimPrefix(images[0].ID, "sha256:")
	if decoded, err := hex.DecodeString(imageID); err != nil || len(decoded) != 32 {
		return errors.New("image inspection did not return a SHA-256 image ID")
	}
	r.ImageID = "sha256:" + imageID
	if err := saveRecord(r); err != nil {
		return err
	}

	memory := strconv.FormatInt(r.Request.MemoryMiB*1024*1024, 10)
	// shortcut: CPU IDs are pinned, not reserved; add reservations with worker scheduling.
	args := []string{
		"run", "--detach", "--pull=never", "--name", r.ContainerName,
		"--cpuset-cpus", r.Request.CPUs, "--memory", memory, "--memory-swap", memory,
		"--network=none", "--image-volume=ignore", "--log-driver=k8s-file",
		"--", r.ImageID,
	}
	args = append(args, r.Request.Args...)
	containerID, err := podman.output(ctx, args...)
	if err != nil {
		return err
	}
	r.ContainerID = strings.TrimSpace(string(containerID))
	r.State = "running"
	if err := saveRecord(r); err != nil {
		return err
	}

	if _, err := podman.output(ctx, "wait", "--", r.ContainerName); err != nil {
		return err
	}
	inspection, err := podman.output(ctx, "container", "inspect", "--format=json", "--", r.ContainerName)
	if err != nil {
		return err
	}
	var containers []struct {
		State struct {
			Status     string
			ExitCode   *int
			OOMKilled  bool
			StartedAt  time.Time
			FinishedAt time.Time
		}
	}
	if err := json.Unmarshal(inspection, &containers); err != nil {
		return fmt.Errorf("decode container inspection: %w", err)
	}
	if len(containers) != 1 || containers[0].State.Status != "exited" {
		return errors.New("container has not exited; retaining it for inspection")
	}
	state := containers[0].State
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
	r.State = "finished"
	if err := saveRecord(r); err != nil {
		return err
	}

	// Keep the container unless its logs and final metadata have reached disk.
	if _, err := podman.output(ctx, "rm", "--", r.ContainerName); err != nil {
		return err
	}
	r.ContainerRemoved = true
	return saveRecord(r)
}

func checkHost(data []byte) error {
	var info struct {
		Host struct {
			OS                string
			ServiceIsRemote   bool
			CgroupVersion     string
			CgroupControllers []string
			Security          struct{ Rootless bool }
		}
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("decode Podman info: %w", err)
	}
	if info.Host.OS != "linux" || info.Host.ServiceIsRemote || !info.Host.Security.Rootless {
		return errors.New("execution requires local, rootless Podman on Linux")
	}
	if info.Host.CgroupVersion != "v2" {
		return errors.New("execution requires cgroup v2 for rootless resource limits")
	}
	for _, controller := range []string{"cpuset", "memory"} {
		if !slices.Contains(info.Host.CgroupControllers, controller) {
			return fmt.Errorf("missing delegated %s cgroup controller; configure delegation for the worker's user session", controller)
		}
	}
	return nil
}

func execPodman(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "podman", append([]string{"--remote=false"}, args...)...)
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

func (podman command) output(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	if err := podman(ctx, &stdout, &stderr, args...); err != nil {
		return nil, fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func collectLogs(ctx context.Context, r *record, podman command) error {
	stdout, err := os.OpenFile(filepath.Join(r.Request.OutputDir, "stdout.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	stderr, err := os.OpenFile(filepath.Join(r.Request.OutputDir, "stderr.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.Join(err, stdout.Close())
	}
	err = podman(ctx, stdout, stderr, "logs", "--", r.ContainerName)
	return errors.Join(err, stdout.Sync(), stderr.Sync(), stdout.Close(), stderr.Close())
}

func saveRecord(r *record) error {
	file, err := os.CreateTemp(r.Request.OutputDir, ".run-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(file.Name()) }()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	err = encoder.Encode(r)
	if err := errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(r.Request.OutputDir, "run.json")); err != nil {
		return err
	}
	dir, err := os.Open(r.Request.OutputDir)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
