package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/r3clab/Ordo/internal/ssh"
	"github.com/r3clab/Ordo/internal/worker"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
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

var commands = map[string]func(context.Context, []string, io.Writer) error{
	"server": serverCommand,
	"submit": submitCommand,
	"schema": schemaCommand,
	"queue":  queueCommand,
}

func dispatch(ctx context.Context, args []string, output io.Writer) error {
	// Internal subprocesses: supervise SSH locally and execute worker operations remotely.
	if len(args) > 1 && args[0] == "__ssh" {
		return ssh.Supervise(ctx, args[1:])
	}
	if len(args) == 1 && args[0] == "worker" {
		ctx, stop := signal.NotifyContext(ctx, syscall.SIGHUP)
		defer stop()
		return worker.Serve(ctx, os.Stdin, output)
	}
	flags := newFlags("ordo")
	err := flags.Parse(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		return err
	}
	if err == nil && flags.NArg() != 0 {
		command, ok := commands[flags.Arg(0)]
		if !ok {
			return fmt.Errorf("unknown command %q", flags.Arg(0))
		}
		return command(ctx, flags.Args()[1:], output)
	}

	_, err = fmt.Fprintln(output, "Usage: ordo <command> [flags]\n\nCommands:\n  server   Run the durable queue and worker reconciliation\n  submit   Queue JSONL jobs, each delivering its results into its out (submit --help for options)\n  queue    Inspect, stop or cancel jobs\n  schema   Print the JSON Schema for jobs")
	return err
}

func newFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func printJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
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
