package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRunCommandHelp(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"run", "--help"}, &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--image", "--cpus", "--memory-mib", "--output", "copyToPodman", "possibly running"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("help does not contain %q", want)
		}
	}
	if err := run(context.Background(), []string{"run", "--help"}, failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("got %v, want output error", err)
	}
}

func TestRunCommandRejectsMissingAndUnknownFlags(t *testing.T) {
	for _, args := range [][]string{{"run"}, {"run", "--unknown"}, {"run", "--image"}} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output); err == nil {
			t.Errorf("run(%q) succeeded unexpectedly", args)
		}
		if output.Len() != 0 {
			t.Errorf("run(%q) wrote unexpected output %q", args, output.String())
		}
	}
}
