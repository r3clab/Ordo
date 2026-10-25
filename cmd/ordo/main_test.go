package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRunHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output bytes.Buffer
			if err := run(args, &output); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"Usage: ordo", "scheduling is not implemented yet"} {
				if !strings.Contains(output.String(), want) {
					t.Errorf("output %q does not contain %q", output.String(), want)
				}
			}
		})
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func TestRunPropagatesOutputErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}} {
		if err := run(args, failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("run(%q): got %v, want %v", args, err, io.ErrClosedPipe)
		}
	}
}

func TestRunRejectsUnknownArguments(t *testing.T) {
	for _, arg := range []string{"submit", "--unknown"} {
		t.Run(arg, func(t *testing.T) {
			var output bytes.Buffer
			if err := run([]string{arg}, &output); err == nil {
				t.Fatal("expected an error")
			}
			if output.Len() != 0 {
				t.Errorf("unexpected output: %q", output.String())
			}
		})
	}
}
