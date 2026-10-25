package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/r3clab/Ordo/internal/task"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		var taskExit *task.ExitError
		if errors.As(err, &taskExit) {
			os.Exit(taskExit.Code)
		}
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		os.Exit(2)
	}
}

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("ordo", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		return err
	}
	if err == nil && flags.NArg() != 0 {
		switch flags.Arg(0) {
		case "run":
			return runTask(ctx, flags.Args()[1:], output)
		case "recover":
			return recoverTask(ctx, flags.Args()[1:], output)
		case "agent":
			return agentCommand(ctx, flags.Args()[1:], output)
		case "worker":
			return workerCommand(ctx, flags.Args()[1:], output)
		}
		return fmt.Errorf("unknown command %q", flags.Arg(0))
	}

	_, err = fmt.Fprintln(output, "Usage: ordo <command> [flags]\n\nCommands:\n  run      Run one local task in rootless Podman (run --help for options)\n  recover  Recover a retained local task (recover --help for options)\n  agent    Serve a persistent worker (agent --help for options)\n  worker   Bootstrap or contact a worker through SSH (worker --help for options)\n\nDistributed scheduling is not implemented yet.")
	return err
}
