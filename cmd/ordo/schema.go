package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/r3clab/Ordo/internal/schema"
)

func schemaCommand(_ context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("schema", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	var dir string
	f.StringVar(&dir, "out", "", "write every schema into this directory")
	if err := parse(f, args, out, fmt.Sprintf("Usage: ordo schema [%s] | ordo schema --out DIR", strings.Join(schema.Names(), "|"))); err != nil {
		return err
	}
	if dir != "" {
		if f.NArg() != 0 {
			return errors.New("--out writes every schema; omit the name")
		}
		return schema.Write(dir)
	}
	name := "request"
	if f.NArg() > 1 {
		return errors.New("name at most one schema")
	} else if f.NArg() == 1 {
		name = f.Arg(0)
	}
	data, err := schema.Generate(name)
	if err != nil {
		return err
	}
	_, err = out.Write(data)
	return err
}
