package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/r3clab/Ordo/internal/worker"
)

func workerCommand(ctx context.Context, args []string, out io.Writer) error {
	usage := "Usage: ordo worker <start|info|submit|get> --host HOST --state REMOTE_DIR --cpus CPU_IDS --memory-mib POOL_MIB [flags]\n\nstart: upload --binary FILE (defaults to this executable) and start/reuse a persistent user service.\nsubmit: --id ID --image sha256:ID --cores N --task-memory-mib MIB [-- COMMAND ARGS...]\nget: --id ID. info: report capabilities and held reservations.\nAll control traffic uses a private SSH-forwarded Unix socket. Closing the client does not stop tasks."
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		_, err := fmt.Fprintln(out, usage)
		return err
	}
	operation := args[0]
	var remote worker.Remote
	var id string
	var spec worker.Spec
	flags := flag.NewFlagSet("worker", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&remote.Host, "host", "", "SSH alias")
	flags.StringVar(&remote.StateDir, "state", "", "absolute private remote state directory")
	flags.StringVar(&remote.CPUs, "cpus", "", "logical CPU pool including complete SMT sibling sets")
	flags.Int64Var(&remote.MemoryMiB, "memory-mib", 0, "aggregate worker memory budget")
	flags.StringVar(&remote.Binary, "binary", "", "static Linux agent executable")
	flags.StringVar(&id, "id", "", "stable attempt ID")
	flags.StringVar(&spec.Image, "image", "", "immutable image ID already present on worker")
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
	case "start", "info", "submit", "get":
	default:
		return fmt.Errorf("unknown worker command %q", operation)
	}
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
	case "submit":
		spec.Args = flags.Args()
		var attempt worker.Attempt
		if id == "" {
			return errors.New("--id is required")
		}
		if err := worker.Call(ctx, tunnel.Client, http.MethodPut, "/v1/attempts/"+url.PathEscape(id), spec, &attempt); err != nil {
			return err
		}
		result = attempt
	case "get":
		if id == "" {
			return errors.New("--id is required")
		}
		var attempt worker.Attempt
		if err := worker.Call(ctx, tunnel.Client, http.MethodGet, "/v1/attempts/"+url.PathEscape(id), nil, &attempt); err != nil {
			return err
		}
		result = attempt
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
