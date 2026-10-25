package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/r3clab/Ordo/internal/task"
)

const runUsage = `Usage: ordo run --image IMAGE --cpus CPU_IDS --memory-mib MIB --output DIR [-- COMMAND ARGS...]

Run a local nix2container image already loaded with copyToPodman.
--cpus accepts CPU IDs and ranges, such as 0,2-3. CPU IDs are pinned, not reserved.
DIR must not exist. Results are saved as run.json, stdout.log, and stderr.log.
Trailing arguments use Podman's ENTRYPOINT/CMD semantics; omit them to use image defaults.
No registry pulls, networking, GPU allocation, or file artifact export.
On interruption or collection failure the container is retained, possibly running.
Use ordo recover --output DIR to collect a retained task, or container_name in run.json
with podman inspect, logs, or stop to manage it manually.`

func runTask(ctx context.Context, args []string, output io.Writer) error {
	var req task.Request
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&req.Image, "image", "", "local image reference")
	flags.StringVar(&req.CPUs, "cpus", "", "CPU IDs or ranges")
	flags.Int64Var(&req.MemoryMiB, "memory-mib", 0, "memory limit in MiB, without swap")
	flags.StringVar(&req.OutputDir, "output", "", "new results directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(output, runUsage)
		}
		return err
	}
	req.Args = flags.Args()
	if err := task.Run(ctx, req); err != nil {
		return err
	}
	_, err := fmt.Fprintf(output, "Results saved to %s (unmanaged CPU pinning; no exclusive reservation)\n", req.OutputDir)
	return err
}
