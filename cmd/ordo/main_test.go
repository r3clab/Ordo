package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output bytes.Buffer
			if err := run(context.Background(), args, &output); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Usage: ordo", "server", "queue"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("output %q does not contain %q", output.String(), want)
				}
			}
		})
	}
}

func TestSubcommandHelp(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"queue", "list", "--help"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage: ordo queue") {
		t.Errorf("output %q lacks queue usage", output.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestRunPropagatesOutputErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}} {
		if err := run(context.Background(), args, failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("run(%q): got %v, want %v", args, err, io.ErrClosedPipe)
		}
	}
}

func TestRunRejectsUnknownArguments(t *testing.T) {
	for _, arg := range []string{"submit", "--unknown"} {
		t.Run(arg, func(t *testing.T) {
			var output bytes.Buffer
			if err := run(context.Background(), []string{arg}, &output); err == nil {
				t.Fatal("expected an error")
			}
			if output.Len() != 0 {
				t.Errorf("unexpected output: %q", output.String())
			}
		})
	}
}
