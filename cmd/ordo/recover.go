package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/r3clab/Ordo/internal/task"
)

const recoverUsage = `Usage: ordo recover --output DIR

Recover a retained local task using DIR/run.json on the same host and Podman user.
Wait for the existing container if running, collect results, then remove it.
Never creates, starts, or restarts a container. Completed runs return their saved exit code.
Do not remove DIR/.lock; only one Ordo process may manage a run directory at a time.
On interruption or collection failure the container is retained, possibly running.`

func recoverTask(ctx context.Context, args []string, output io.Writer) error {
	var dir string
	flags := flag.NewFlagSet("recover", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&dir, "output", "", "existing results directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(output, recoverUsage)
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("recover accepts no positional arguments")
	}
	if err := task.Recover(ctx, dir); err != nil {
		return err
	}
	_, err := fmt.Fprintf(output, "Results saved to %s\n", dir)
	return err
}
