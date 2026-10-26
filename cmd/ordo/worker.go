package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/task"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/r3clab/Ordo/internal/worker"
)

func workerCommand(ctx context.Context, args []string, out io.Writer) error {
	usage := "Usage: ordo worker <start|info|submit|get|put-image|fetch|cancel> --host HOST --state REMOTE_DIR --cpus CPU_IDS --memory-mib POOL_MIB [flags]\n\nstart: upload --binary FILE (defaults to this executable) and start/reuse a persistent user service.\nsubmit: --id ID --image sha256:ID --cores N --task-memory-mib MIB [-- COMMAND ARGS...]\nget: --id ID. info: report capabilities and held reservations.\nput-image: --image LOCAL_REFERENCE, or --image sha256:ID --archive OCI_FILE.\nsubmit: optional repeated --input NAME=DIR. fetch: --id ID --output NEW_DIR.\nAll control traffic uses a private SSH-forwarded Unix socket. Closing the client does not stop tasks."
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, usage)
		return err
	}
	operation := args[0]
	var remote worker.Remote
	var id, archive, outputDir string
	inputs := map[string]string{}
	var spec worker.Spec
	var match worker.Match
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&remote.Host, "host", "", "SSH alias")
	flags.StringVar(&remote.StateDir, "state", "", "absolute private remote state directory")
	flags.StringVar(&remote.CPUs, "cpus", "", "logical CPU pool including complete SMT sibling sets")
	flags.Int64Var(&remote.MemoryMiB, "memory-mib", 0, "aggregate worker memory budget")
	flags.Func("gpus", "allowed worker GPU UUID pool", func(value string) error { ids, err := worker.GPUList(value); remote.GPUs = ids; return err })
	flags.StringVar(&match.Arch, "arch", "", "required worker architecture (amd64/arm64)")
	flags.StringVar(&match.CPUModel, "cpu-model", "", "exact CPU model requirement")
	flags.StringVar(&match.GPUModel, "gpu-model", "", "exact GPU model requirement")
	flags.StringVar(&match.GPUDriver, "gpu-driver", "", "exact GPU driver requirement")
	flags.Int64Var(&match.MinGPUMemoryMiB, "min-gpu-memory-mib", 0, "minimum memory per GPU")
	flags.IntVar(&spec.Resources.GPUs, "task-gpus", 0, "exclusive GPU devices")
	flags.StringVar(&remote.Binary, "binary", "", "static Linux agent executable")
	flags.StringVar(&id, "id", "", "stable attempt ID")
	flags.StringVar(&archive, "archive", "", "OCI archive for put-image; otherwise export a local image")
	flags.StringVar(&outputDir, "output", "", "new local result directory for fetch")
	flags.Func("input", "NAME=local-directory (repeatable)", func(value string) error {
		name, dir, ok := strings.Cut(value, "=")
		if !ok || name == "" || dir == "" {
			return errors.New("input must be NAME=DIR")
		}
		if _, exists := inputs[name]; exists {
			return errors.New("duplicate input")
		}
		inputs[name] = dir
		return nil
	})
	flags.StringVar(&spec.Image, "image", "", "immutable image ID already present on worker")
	flags.Int64Var(&spec.TimeoutSeconds, "timeout-seconds", 0, "per-attempt wall-clock deadline; zero disables")
	flags.IntVar(&spec.Resources.Cores, "cores", 0, "physical cores")
	flags.Int64Var(&spec.Resources.MemoryMiB, "task-memory-mib", 0, "task memory limit")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(out, usage)
		}
		return err
	}
	if operation != "submit" && flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	switch operation {
	case "start", "info", "put-image", "fetch":
	case "submit", "get", "cancel":
		if id == "" {
			return errors.New("--id is required")
		}
	default:
		return fmt.Errorf("unknown worker command %q", operation)
	}
	attempt := "/v1/attempts/" + url.PathEscape(id)
	if operation == "start" {
		if err := worker.Bootstrap(ctx, remote); err != nil {
			return err
		}
	}
	tunnel, err := worker.Connect(ctx, remote)
	if err != nil {
		return err
	}
	defer func() { _ = tunnel.Close() }()
	var result any
	switch operation {
	case "start", "info":
		result = tunnel.Info
	case "put-image":
		if archive == "" {
			dir, err := os.MkdirTemp("", "ordo-image-")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(dir) }()
			archive = filepath.Join(dir, "image.tar")
			spec.Image, err = task.ExportImage(ctx, spec.Image, archive)
			if err != nil {
				return err
			}
		}
		digest, err := content.Hash(archive)
		if err != nil {
			return err
		}
		if err := worker.EnsureImage(ctx, tunnel.Client, archive, digest, spec.Image); err != nil {
			return err
		}
		result = map[string]string{"image": spec.Image}
	case "fetch":
		if outputDir == "" {
			return errors.New("--output is required")
		}
		if err := worker.FetchResult(ctx, tunnel.Client, id, outputDir); err != nil {
			return err
		}
		result = map[string]string{"output": outputDir}
	case "submit":
		if match != (worker.Match{}) {
			spec.Match = &match
		}
		spec.Inputs = map[string]string{}
		for name, source := range inputs {
			dir, err := os.MkdirTemp("", "ordo-input-")
			if err != nil {
				return err
			}
			defer func() { _ = os.RemoveAll(dir) }()
			archive := filepath.Join(dir, "input.tar")
			hash, err := content.Archive(source, archive)
			if err != nil {
				return err
			}
			if err := worker.EnsureInput(ctx, tunnel.Client, archive, hash); err != nil {
				return err
			}
			spec.Inputs[name] = hash
		}
		spec.Args = flags.Args()
		var run worker.Attempt
		if err := worker.Call(ctx, tunnel.Client, http.MethodPut, attempt, spec, &run); err != nil {
			return err
		}
		result = run
	case "cancel":
		if err := worker.Call(ctx, tunnel.Client, http.MethodPost, attempt+"/cancel", nil, &result); err != nil {
			return err
		}
	case "get":
		var run worker.Attempt
		if err := worker.Call(ctx, tunnel.Client, http.MethodGet, attempt, nil, &run); err != nil {
			return err
		}
		result = run
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
