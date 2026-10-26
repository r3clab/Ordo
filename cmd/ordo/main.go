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
	if err := dispatch(ctx, args, output); !errors.Is(err, flag.ErrHelp) {
		return err
	}
	return nil
}

func dispatch(ctx context.Context, args []string, output io.Writer) error {
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
		case "server":
			return serverCommand(ctx, flags.Args()[1:], output)
		case "submit":
			return submitCommand(ctx, flags.Args()[1:], output)
		case "fetch":
			return fetchCommand(ctx, flags.Args()[1:], output)
		case "queue":
			return queueCommand(ctx, flags.Args()[1:], output)
		case "worker":
			return workerCommand(ctx, flags.Args()[1:], output)
		}
		return fmt.Errorf("unknown command %q", flags.Arg(0))
	}

	_, err = fmt.Fprintln(output, "Usage: ordo <command> [flags]\n\nCommands:\n  run      Run one local task in rootless Podman (run --help for options)\n  recover  Recover a retained local task (recover --help for options)\n  agent    Serve a persistent worker (agent --help for options)\n  worker   Bootstrap or contact a worker through SSH (worker --help for options)\n  server   Run the durable queue and worker reconciliation\n  submit   Queue JSONL job requests from stdin (submit --help for options)\n  fetch    Download finished job results into a directory (fetch --help for options)\n  queue    Submit workflows, inspect, stop or cancel queued jobs")
	return err
}

func parse(flags *flag.FlagSet, args []string, out io.Writer, usage string) error {
	err := flags.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		if _, printErr := fmt.Fprintln(out, usage); printErr != nil {
			return printErr
		}
	}
	return err
}
