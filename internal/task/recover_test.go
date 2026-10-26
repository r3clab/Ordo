package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func retainedRun(t *testing.T, stage string, exitCode int) (Request, *fakePodman) {
	t.Helper()
	req := request(t)
	p := &fakePodman{failAt: stage, failure: context.Canceled, exitCode: exitCode}
	if err := run(context.Background(), req, p.command); !errors.Is(err, context.Canceled) {
		t.Fatalf("prepare retained run: %v", err)
	}
	p.name = readRecord(t, req.OutputDir).ContainerName
	p.calls, p.failAt, p.failure = nil, "", nil
	return req, p
}

func wantExit(t *testing.T, err error, code int) {
	t.Helper()
	if code == 0 {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != code {
		t.Fatalf("got %v, want task exit %d", err, code)
	}
}

func TestRecoverCollectsWithoutRerunning(t *testing.T) {
	for _, stage := range []string{"run", "wait", "logs"} {
		for _, code := range []int{0, 7, 137} {
			t.Run(fmt.Sprintf("%s/%d", stage, code), func(t *testing.T) {
				req, p := retainedRun(t, stage, code)
				for _, name := range []string{"stdout.log", "stderr.log"} {
					if err := os.WriteFile(filepath.Join(req.OutputDir, name), []byte("partial"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				// Resource delegation may have changed since launch; recovery needs no new allocation.
				p.info = strings.Replace(readyHost, `"cpuset",`, "", 1)
				p.onCall = func(args []string) {
					switch args[0] {
					case "info", "container":
					case "wait", "logs", "rm":
						if args[len(args)-1] != strings.Repeat("b", 64) {
							t.Fatalf("operation did not target immutable container ID: %q", args)
						}
					default:
						t.Fatalf("recovery attempted unexpected command: %q", args)
					}
					if args[0] == "rm" {
						r := readRecord(t, req.OutputDir)
						if r.State != "finished" || r.ExitCode == nil || *r.ExitCode != code {
							t.Fatalf("removal before final metadata: %+v", r)
						}
						for _, stream := range []string{"stdout", "stderr"} {
							data, err := os.ReadFile(filepath.Join(req.OutputDir, stream+".log"))
							if err != nil || string(data) != "task "+stream+"\n" {
								t.Fatalf("removal before complete logs: %q, %v", data, err)
							}
						}
					}
				}
				wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), code)
				r := readRecord(t, req.OutputDir)
				if !r.ContainerRemoved || r.Error != "" || r.ImageID != "sha256:"+testImageID {
					t.Fatalf("invalid recovered result: %+v", r)
				}
				calls := len(p.calls)
				wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), code)
				if len(p.calls) != calls {
					t.Fatal("completed recovery contacted Podman")
				}
			})
		}
	}
}

func TestRecoverWaitsForRunningContainer(t *testing.T) {
	req, p := retainedRun(t, "wait", 137)
	p.inspection = p.containerJSON("running")
	p.onCall = func(args []string) {
		if args[0] == "wait" {
			p.inspection = strings.Replace(p.containerJSON("exited"), `"ExitCode":137`, `"ExitCode":137,"OOMKilled":true`, 1)
		}
	}
	wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), 137)
	if !p.called("wait") || !readRecord(t, req.OutputDir).OOMKilled {
		t.Fatal("did not wait and collect the final OOM status")
	}
}

func TestRecoverCleanupAfterCrash(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			req, p := retainedRun(t, "rm", 7)
			p.missing = missing
			wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), 7)
			if p.called("logs") || p.called("wait") || p.called("rm") == missing {
				t.Fatalf("unexpected cleanup calls: %q", p.calls)
			}
			if !readRecord(t, req.OutputDir).ContainerRemoved {
				t.Fatal("cleanup not persisted")
			}
		})
	}
}

func TestRecoverMissingContainerDoesNotRerun(t *testing.T) {
	req, p := retainedRun(t, "wait", 0)
	p.missing = true
	err := recoverRun(context.Background(), req.OutputDir, p.command)
	if err == nil || !strings.Contains(err.Error(), "refusing to rerun") || p.called("run") || p.called("rm") {
		t.Fatalf("missing task was not rejected: %v, %q", err, p.calls)
	}
}

func TestRecoverStorageFailureIsNotAbsence(t *testing.T) {
	for _, failure := range []error{commandExit(125), commandExit(2), context.Canceled} {
		req, p := retainedRun(t, "rm", 0)
		p.failAt, p.failure = "container", failure
		if err := recoverRun(context.Background(), req.OutputDir, p.command); !errors.Is(err, failure) {
			t.Fatalf("got %v, want %v", err, failure)
		}
		r := readRecord(t, req.OutputDir)
		if r.State != "finished" || r.ContainerRemoved || p.called("rm") {
			t.Fatalf("storage failure treated as successful cleanup: %+v", r)
		}
	}
}

func TestRecoverRejectsWrongContainerOrState(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"ID":        func(s string) string { return strings.Replace(s, strings.Repeat("b", 64), strings.Repeat("c", 64), 1) },
		"image":     func(s string) string { return strings.Replace(s, testImageID, strings.Repeat("c", 64), 1) },
		"name":      func(s string) string { return strings.Replace(s, `"Name":"ordo-`, `"Name":"other-`, 1) },
		"created":   func(s string) string { return strings.Replace(s, `"exited"`, `"created"`, 1) },
		"paused":    func(s string) string { return strings.Replace(s, `"exited"`, `"paused"`, 1) },
		"malformed": func(string) string { return "not JSON" },
	} {
		t.Run(name, func(t *testing.T) {
			req, p := retainedRun(t, "wait", 0)
			p.inspection = mutate(p.containerJSON("exited"))
			if err := recoverRun(context.Background(), req.OutputDir, p.command); err == nil || p.called("wait") || p.called("logs") || p.called("rm") {
				t.Fatalf("unsafe recovery: %v, %q", err, p.calls)
			}
		})
	}
}

func TestRecoverRequiresLabelOnlyWhenIDWasNotSaved(t *testing.T) {
	for _, stage := range []string{"run", "wait"} {
		req, p := retainedRun(t, stage, 0)
		p.inspection = strings.Replace(p.containerJSON("exited"), runLabel, "unrelated.label", 1)
		err := recoverRun(context.Background(), req.OutputDir, p.command)
		if stage == "run" {
			if err == nil || p.called("wait") || p.called("rm") {
				t.Fatalf("adopted unproven container: %v", err)
			}
		} else {
			wantExit(t, err, 0) // Records from before Ordo labels still have immutable IDs.
		}
	}
}

func TestRecoverRejectsChangedExecution(t *testing.T) {
	for _, stage := range []string{"logs", "rm"} {
		req, p := retainedRun(t, stage, 0)
		p.inspection = strings.Replace(p.containerJSON("exited"), "00:00:00Z", "00:00:10Z", 1)
		if err := recoverRun(context.Background(), req.OutputDir, p.command); err == nil || p.called("logs") || p.called("rm") {
			t.Fatalf("accepted restarted container: %v", err)
		}
	}
}

func TestRecoverPreservesPartialLogsOnCollectionFailure(t *testing.T) {
	req, p := retainedRun(t, "logs", 0)
	for _, name := range []string{"stdout.log", "stderr.log"} {
		if err := os.WriteFile(filepath.Join(req.OutputDir, name), []byte("previous partial output"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	failingCollection := func(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
		err := p.command(ctx, stdout, stderr, args...)
		if args[0] == "logs" {
			return context.Canceled
		}
		return err
	}
	if err := recoverRun(context.Background(), req.OutputDir, failingCollection); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		data, err := os.ReadFile(filepath.Join(req.OutputDir, name))
		if err != nil || string(data) != "previous partial output" {
			t.Fatalf("partial logs were destroyed: %q, %v", data, err)
		}
	}
	if p.called("rm") || readRecord(t, req.OutputDir).State != "interrupted" {
		t.Fatal("collection failure did not retain the interrupted task")
	}
	wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), 0)
}

func TestRecoverRejectsInvalidMetadataWithoutChangingIt(t *testing.T) {
	for name, change := range map[string]func(*Record){
		"unresolved image":     func(r *Record) { r.ImageID = "" },
		"invalid container ID": func(r *Record) { r.ContainerID = "tag" },
		"invalid name":         func(r *Record) { r.ContainerName = "other-container" },
		"invalid request":      func(r *Record) { r.Request.MemoryMiB = 0 },
		"unknown state":        func(r *Record) { r.State = "unknown" },
		"invalid exit":         func(r *Record) { r.ExitCode = new(256) },
		"missing exit":         func(r *Record) { r.State = "finished"; r.ExitCode = nil },
		"removed but running":  func(r *Record) { r.State = "running"; r.ContainerRemoved = true; r.ExitCode = new(0) },
	} {
		t.Run(name, func(t *testing.T) {
			req, p := retainedRun(t, "wait", 0)
			r := readRecord(t, req.OutputDir)
			change(&r)
			if err := saveRecord(&r); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(req.OutputDir, "run.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := recoverRun(context.Background(), req.OutputDir, p.command); err == nil || len(p.calls) != 0 {
				t.Fatalf("invalid record used: %v, %q", err, p.calls)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) {
				t.Fatal("invalid metadata modified")
			}
		})
	}
}

func TestRecoverUsesSelectedDirectory(t *testing.T) {
	req, p := retainedRun(t, "wait", 0)
	r := readRecord(t, req.OutputDir)
	other := t.TempDir()
	r.Request.OutputDir = other
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(req.OutputDir, "run.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), 0)
	entries, err := os.ReadDir(other)
	if err != nil || len(entries) != 0 {
		t.Fatalf("embedded path used: %v, %v", entries, err)
	}
	if readRecord(t, req.OutputDir).Request.OutputDir != req.OutputDir {
		t.Fatal("selected path not saved")
	}
}

func TestRunAndRecoveryLockDirectory(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		req := request(t)
		p := &fakePodman{}
		if recovering {
			req, p = retainedRun(t, "wait", 0)
		}
		p.onCall = func(args []string) {
			if args[0] != "wait" {
				return
			}
			other := &fakePodman{}
			err := recoverRun(context.Background(), req.OutputDir, other.command)
			if !errors.Is(err, syscall.EWOULDBLOCK) || len(other.calls) != 0 {
				t.Fatalf("concurrent recovery accepted: %v", err)
			}
		}
		if recovering {
			wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), 0)
		} else {
			wantExit(t, run(context.Background(), req, p.command), 0)
		}
		wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), 0)
	}
}

func TestRecoverRejectsMissingSavedLogs(t *testing.T) {
	req, p := retainedRun(t, "rm", 0)
	if err := os.Remove(filepath.Join(req.OutputDir, "stdout.log")); err != nil {
		t.Fatal(err)
	}
	if err := recoverRun(context.Background(), req.OutputDir, p.command); err == nil || p.called("rm") {
		t.Fatalf("discarded container without saved logs: %v", err)
	}
}

func TestRecoverLegacyPostRemovalError(t *testing.T) {
	req := request(t)
	p := &fakePodman{}
	wantExit(t, run(context.Background(), req, p.command), 0)
	r := readRecord(t, req.OutputDir)
	r.State, r.Error = "error", "old cleanup metadata error"
	if err := saveRecord(&r); err != nil {
		t.Fatal(err)
	}
	p.calls = nil
	wantExit(t, recoverRun(context.Background(), req.OutputDir, p.command), 0)
	if len(p.calls) != 0 || readRecord(t, req.OutputDir).State != "finished" {
		t.Fatal("legacy completed record was not recovered locally")
	}
}
