package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/r3clab/Ordo/internal/worker"
)

func agentCommand(ctx context.Context, args []string, out io.Writer) error {
	var cfg worker.Config
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.StateDir, "state", "", "private local state directory")
	flags.StringVar(&cfg.CPUs, "cpus", "", "allowed logical CPUs, including every SMT sibling")
	flags.Int64Var(&cfg.MemoryMiB, "memory-mib", 0, "aggregate task memory budget")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintln(out, "Usage: ordo agent --state DIR --cpus CPU_IDS --memory-mib MIB\n\nServe a private Unix socket at DIR/agent.sock. Reserve whole physical cores.\nRun under a persistent user service; disconnect/shutdown never cancels tasks.")
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("agent accepts no positional arguments")
	}
	agent, err := worker.Open(ctx, cfg)
	if err != nil {
		return err
	}
	err = agent.Serve()
	return errors.Join(err, agent.Close())
}
