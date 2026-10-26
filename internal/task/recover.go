package task

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r3clab/Ordo/internal/content"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const runLabel = "io.r3clab.ordo.run"

func Recover(ctx context.Context, outputDir string) error {
	return recoverRun(ctx, outputDir, execPodman)
}

func recoverRun(ctx context.Context, outputDir string, podman command) error {
	if outputDir == "" {
		return errors.New("--output is required")
	}
	outputDir, err := filepath.Abs(outputDir)
	if err != nil {
		return err
	}
	lock, err := lockRun(outputDir)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	// A crash during launch can leave the secrets file behind; the container no longer needs it.
	if err := removeSecrets(outputDir); err != nil {
		return err
	}
	r, err := loadRecord(outputDir)
	if err != nil {
		return err
	}
	return outcome(r, reconcile(ctx, r, podman))
}

func reconcile(ctx context.Context, r *Record, podman command) error {
	if r.ContainerRemoved {
		if err := savedResults(r); err != nil {
			return err
		}
		r.State = "finished"
		r.Error = ""
		return saveRecord(r)
	}
	info, err := podman.output(ctx, "info", "--format=json")
	if err != nil {
		return err
	}
	if _, err := readLocalHost(info); err != nil {
		return err
	}
	selector := cmp.Or(r.ContainerID, r.ContainerName)
	exists, err := podman.containerExists(ctx, selector)
	if err != nil {
		return err
	}
	if !exists {
		if r.State != "finished" {
			return errors.New("container is missing and results were not marked finished; refusing to rerun the task")
		}
		if err := savedResults(r); err != nil {
			return err
		}
		r.ContainerRemoved = true
		r.Error = ""
		return saveRecord(r)
	}
	container, err := podman.inspectOwned(ctx, r, selector)
	if err != nil {
		return err
	}
	r.ContainerID = container.ID
	if r.State == "finished" {
		if err := savedResults(r); err != nil {
			return err
		}
		if container.State.Status != "exited" || container.State.ExitCode == nil || *container.State.ExitCode != *r.ExitCode {
			return errors.New("container state conflicts with the saved results; refusing cleanup")
		}
		return cleanup(ctx, r, podman)
	}
	if container.State.Status != "running" && container.State.Status != "exited" {
		return fmt.Errorf("cannot recover container in state %q; it will not be started or restarted", container.State.Status)
	}
	r.State = "running"
	r.Error = ""
	if err := saveRecord(r); err != nil {
		return err
	}
	return complete(ctx, r, podman)
}

func loadRecord(dir string) (*Record, error) {
	r, err := ReadResult(dir)
	if err != nil {
		return nil, err
	}
	return checkRecord(dir, r)
}

func checkRecord(dir string, r Record) (*Record, error) {
	// The selected directory, not an embedded path, owns recovery's file writes.
	r.Request.OutputDir = dir
	if err := r.Request.validate(); err != nil {
		return nil, fmt.Errorf("invalid saved request: %w", err)
	}
	if !strings.HasPrefix(r.ContainerName, "ordo-") || !ValidImageID(r.ImageID) {
		return nil, errors.New("run has no valid container name and resolved image identity; no task can be recovered")
	}
	if r.ContainerID != "" && !content.Valid(r.ContainerID) {
		return nil, errors.New("invalid saved container ID")
	}
	switch r.State {
	case "preparing", "running", "interrupted", "error", "finished":
	default:
		return nil, fmt.Errorf("invalid saved run state %q", r.State)
	}
	if r.ExitCode != nil && (*r.ExitCode < 0 || *r.ExitCode > 255) {
		return nil, errors.New("invalid saved exit code")
	}
	if (r.State == "finished" || r.ContainerRemoved) && r.ExitCode == nil {
		return nil, errors.New("finished run has no saved exit code")
	}
	if r.ContainerRemoved && r.State != "finished" && r.State != "error" {
		return nil, errors.New("removed container has an inconsistent saved run state")
	}
	return &r, nil
}

func savedResults(r *Record) error {
	if r.Request.ExportOutputs {
		hash, err := content.Hash(filepath.Join(r.Request.OutputDir, "outputs.tar"))
		if err != nil {
			return err
		}
		if !content.Valid(r.OutputsSHA256) || hash != r.OutputsSHA256 {
			return errors.New("saved output archive checksum mismatch")
		}
	}
	if r.ExitCode == nil {
		return errors.New("finished run has no saved exit code")
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		info, err := os.Lstat(filepath.Join(r.Request.OutputDir, name))
		if err != nil {
			return fmt.Errorf("check saved %s: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("saved %s is not a regular file", name)
		}
	}
	return nil
}

func lockRun(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("lock run directory (another Ordo process may be using it): %w", err), file.Close())
	}
	return file, nil
}

type containerInspection struct {
	ID     string
	Name   string
	Image  string
	Config struct{ Labels map[string]string }
	State  struct {
		Status     string
		ExitCode   *int
		OOMKilled  bool
		StartedAt  time.Time
		FinishedAt time.Time
	}
}

func (podman command) inspectOwned(ctx context.Context, r *Record, selector string) (containerInspection, error) {
	data, err := podman.output(ctx, "container", "inspect", "--format=json", "--", selector)
	if err != nil {
		return containerInspection{}, err
	}
	var containers []containerInspection
	if err := json.Unmarshal(data, &containers); err != nil {
		return containerInspection{}, fmt.Errorf("decode container inspection: %w", err)
	}
	if len(containers) != 1 {
		return containerInspection{}, errors.New("expected exactly one container")
	}
	c := containers[0]
	if !content.Valid(c.ID) || c.Name != r.ContainerName || strings.TrimPrefix(c.Image, "sha256:") != strings.TrimPrefix(r.ImageID, "sha256:") {
		return containerInspection{}, errors.New("container identity does not match the run record")
	}
	if r.ContainerID != "" {
		if c.ID != r.ContainerID {
			return containerInspection{}, errors.New("container ID does not match the run record")
		}
	} else if c.Config.Labels[runLabel] != r.ContainerName {
		return containerInspection{}, errors.New("container ID was not saved and the container has no matching Ordo label; inspect it manually")
	}
	if !r.StartedAt.IsZero() && !r.StartedAt.Equal(c.State.StartedAt) {
		return containerInspection{}, errors.New("container start time changed; refusing to collect a different execution")
	}
	return c, nil
}

func (podman command) containerExists(ctx context.Context, selector string) (bool, error) {
	_, err := podman.output(ctx, "container", "exists", "--", selector)
	if err == nil {
		return true, nil
	}
	var status interface{ ExitCode() int }
	if errors.As(err, &status) && status.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}
