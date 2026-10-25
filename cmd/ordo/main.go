package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("ordo", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	err := flags.Parse(args)
	if err != nil && !errors.Is(err, flag.ErrHelp) {
		return err
	}
	if err == nil && flags.NArg() != 0 {
		return fmt.Errorf("unknown command %q", flags.Arg(0))
	}

	_, err = fmt.Fprintln(output, "Usage: ordo [-h | --help]\n\nTask dispatcher for reproducible evaluations.\nDevelopment scaffold: scheduling is not implemented yet.")
	return err
}
