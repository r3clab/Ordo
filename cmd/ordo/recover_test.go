package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/task"
)

func TestRecoverCommandHelp(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"recover", "--help"}, &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--output", "same host", "Never creates", "possibly running"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("help missing %q", want)
		}
	}
	if err := run(context.Background(), []string{"recover", "--help"}, failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("got %v, want output error", err)
	}
}

func TestRecoverCommandRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"recover"}, {"recover", "--unknown"}, {"recover", "--output"}, {"recover", "unexpected"}, {"recover", "--output", "/missing", "extra"}} {
		var output bytes.Buffer
		if err := run(context.Background(), args, &output); err == nil || output.Len() != 0 {
			t.Fatalf("run(%q): error %v, output %q", args, err, output.String())
		}
	}
}

func TestRecoverCommandCompletedResults(t *testing.T) {
	for _, code := range []int{0, 7} {
		dir := t.TempDir()
		data, err := json.Marshal(map[string]any{
			"request":        task.Request{Image: "evaluation:build-hash", CPUs: "0", MemoryMiB: 128, OutputDir: dir},
			"container_name": "ordo-fixture", "container_id": strings.Repeat("b", 64),
			"image_id": "sha256:" + strings.Repeat("a", 64), "state": "finished", "exit_code": code, "container_removed": true,
		})
		if err != nil {
			t.Fatal(err)
		}
		for name, contents := range map[string][]byte{"run.json": data, "stdout.log": {}, "stderr.log": {}} {
			if err := os.WriteFile(filepath.Join(dir, name), contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var output bytes.Buffer
		err = run(context.Background(), []string{"recover", "--output", dir}, &output)
		if code == 0 {
			if err != nil || !strings.Contains(output.String(), dir) {
				t.Fatalf("got %v, %q", err, output.String())
			}
			if err := run(context.Background(), []string{"recover", "--output", dir}, failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
		} else {
			var exit *task.ExitError
			if !errors.As(err, &exit) || exit.Code != code || output.Len() != 0 {
				t.Fatalf("got %v, %q", err, output.String())
			}
		}
	}
}
