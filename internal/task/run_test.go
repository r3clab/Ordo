package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const readyHost = `{"host":{"os":"linux","security":{"rootless":true},"cgroupVersion":"v2","cgroupControllers":["cpu","cpuset","memory"]}}`

var testImageID = strings.Repeat("a", 64)

type fakePodman struct {
	calls      [][]string
	exitCode   int
	failAt     string
	failure    error
	info       string
	image      string
	inspection string
	onCall     func([]string)
	name       string
	missing    bool
}

func (p *fakePodman) command(_ context.Context, stdout, stderr io.Writer, args ...string) error {
	p.calls = append(p.calls, slices.Clone(args))
	if p.onCall != nil {
		p.onCall(args)
	}
	if args[0] == p.failAt {
		if p.failure != nil {
			return p.failure
		}
		return errors.New("simulated Podman failure")
	}
	var text string
	switch args[0] {
	case "info":
		text = readyHost
		if p.info != "" {
			text = p.info
		}
	case "image":
		text = `[{"Id":"` + testImageID + `"}]`
		if p.image != "" {
			text = p.image
		}
	case "run":
		p.name = args[slices.Index(args, "--name")+1]
		text = strings.Repeat("b", 64)
	case "wait":
		text = fmt.Sprint(p.exitCode)
	case "container":
		if args[1] == "exists" {
			if p.missing {
				return commandExit(1)
			}
			return nil
		}
		text = p.containerJSON("exited")
		if p.inspection != "" {
			text = p.inspection
		}
	case "logs":
		_, outErr := io.WriteString(stdout, "task stdout\n")
		_, errErr := io.WriteString(stderr, "task stderr\n")
		return errors.Join(outErr, errErr)
	case "rm":
	default:
		return fmt.Errorf("unexpected command %q", args)
	}
	_, err := io.WriteString(stdout, text)
	return err
}

type commandExit int

func (e commandExit) Error() string { return fmt.Sprintf("command exited with status %d", e) }
func (e commandExit) ExitCode() int { return int(e) }

func (p *fakePodman) containerJSON(status string) string {
	return fmt.Sprintf(`[{"Id":%q,"Name":%q,"Image":%q,"Config":{"Labels":{%q:%q}},"State":{"Status":%q,"ExitCode":%d,"StartedAt":"2026-01-01T00:00:00Z","FinishedAt":"2026-01-01T00:00:01Z"}}]`,
		strings.Repeat("b", 64), p.name, testImageID, runLabel, p.name, status, p.exitCode)
}

func (p *fakePodman) called(name string) bool {
	return slices.ContainsFunc(p.calls, func(args []string) bool { return args[0] == name })
}

func request(t *testing.T) Request {
	t.Helper()
	return Request{Image: "evaluation:build-hash", CPUs: "0,2-3", MemoryMiB: 128, OutputDir: filepath.Join(t.TempDir(), "run")}
}

func readRecord(t *testing.T, dir string) record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r record
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunCollectsBeforeRemovingContainer(t *testing.T) {
	for _, exitCode := range []int{0, 7, 137} {
		t.Run(fmt.Sprint(exitCode), func(t *testing.T) {
			req := request(t)
			req.Args = []string{"argument with spaces", "$(not-a-shell)", "--flag"}
			p := fakePodman{exitCode: exitCode}
			p.onCall = func(args []string) {
				switch args[0] {
				case "run":
					r := readRecord(t, req.OutputDir)
					if r.ContainerName == "" || r.ImageID != "sha256:"+testImageID {
						t.Fatal("container identity and immutable image must be saved before launch")
					}
					expected := []string{
						"run", "--detach", "--pull=never", "--name", r.ContainerName,
						"--label", "io.r3clab.ordo.run=" + r.ContainerName,
						"--cpuset-cpus", req.CPUs, "--memory", "134217728", "--memory-swap", "134217728",
						"--network=none", "--image-volume=ignore", "--log-driver=k8s-file",
						"--", "sha256:" + testImageID,
					}
					expected = append(expected, req.Args...)
					if !slices.Equal(args, expected) {
						t.Fatalf("argv = %q, want %q", args, expected)
					}
				case "rm":
					r := readRecord(t, req.OutputDir)
					if r.State != "finished" || r.ExitCode == nil || *r.ExitCode != exitCode || r.ContainerRemoved {
						t.Fatalf("invalid pre-cleanup record: %+v", r)
					}
					for _, stream := range []string{"stdout", "stderr"} {
						path := filepath.Join(req.OutputDir, stream+".log")
						data, err := os.ReadFile(path)
						if err != nil || string(data) != "task "+stream+"\n" {
							t.Fatalf("%s: %q, %v", path, data, err)
						}
					}
				}
			}
			err := run(context.Background(), req, p.command)
			if exitCode == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var taskExit *ExitError
				if !errors.As(err, &taskExit) || taskExit.Code != exitCode {
					t.Fatalf("got %v, want task exit %d", err, exitCode)
				}
			}
			r := readRecord(t, req.OutputDir)
			if !r.ContainerRemoved || r.Error != "" || r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
				t.Fatalf("invalid final record: %+v", r)
			}
			for path, mode := range map[string]os.FileMode{req.OutputDir: 0o700, filepath.Join(req.OutputDir, "run.json"): 0o600, filepath.Join(req.OutputDir, "stdout.log"): 0o600} {
				info, err := os.Stat(path)
				if err != nil || info.Mode().Perm() != mode {
					t.Fatalf("wrong permissions on %s: %v", path, err)
				}
			}
		})
	}
}

func TestRunRetainsContainerOnInfrastructureFailure(t *testing.T) {
	for _, stage := range []string{"info", "image", "run", "wait", "container", "logs", "rm"} {
		t.Run(stage, func(t *testing.T) {
			req := request(t)
			p := fakePodman{failAt: stage}
			if err := run(context.Background(), req, p.command); err == nil {
				t.Fatal("expected an error")
			}
			r := readRecord(t, req.OutputDir)
			wantState := "error"
			if stage == "rm" {
				wantState = "finished"
			}
			if r.State != wantState || r.Error == "" || r.ContainerRemoved {
				t.Fatalf("invalid failure record: %+v", r)
			}
			if stage != "rm" && p.called("rm") {
				t.Fatal("container removed after an infrastructure failure")
			}
		})
	}
}

func TestRunInterruptionKeepsContainer(t *testing.T) {
	req := request(t)
	p := fakePodman{failAt: "wait", failure: context.Canceled}
	if err := run(context.Background(), req, p.command); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want cancellation", err)
	}
	r := readRecord(t, req.OutputDir)
	if r.State != "interrupted" || r.ContainerID == "" || p.called("rm") || p.called("stop") {
		t.Fatalf("interrupted container was not retained: %+v", r)
	}
}

func TestRunDoesNotOverwriteExistingResults(t *testing.T) {
	req := request(t)
	if err := os.Mkdir(req.OutputDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(req.OutputDir, "run.json")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := fakePodman{}
	if err := run(context.Background(), req, p.command); err == nil || len(p.calls) != 0 {
		t.Fatalf("existing directory accepted: %v", err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep me" {
		t.Fatal("existing results were modified")
	}
}

func TestRunKeepsContainerWhenLogFileCannotBeCreated(t *testing.T) {
	req := request(t)
	p := fakePodman{}
	p.onCall = func(args []string) {
		if args[0] == "container" {
			if err := os.Mkdir(filepath.Join(req.OutputDir, "stdout.log"), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := run(context.Background(), req, p.command); err == nil {
		t.Fatal("expected collection error")
	}
	if p.called("rm") {
		t.Fatal("container removed without collected logs")
	}
}

func TestRunKeepsContainerWhenMetadataCannotBeSaved(t *testing.T) {
	req := request(t)
	p := fakePodman{}
	p.onCall = func(args []string) {
		if args[0] == "logs" {
			path := filepath.Join(req.OutputDir, "run.json")
			if err := os.Rename(path, filepath.Join(req.OutputDir, "previous.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := run(context.Background(), req, p.command); err == nil {
		t.Fatal("expected metadata persistence error")
	}
	if p.called("rm") {
		t.Fatal("container removed without saved final metadata")
	}
}

func TestValidateRequest(t *testing.T) {
	for _, cpus := range []string{"0", "0,2-3", "1-128"} {
		req := request(t)
		req.CPUs = cpus
		if err := req.validate(); err != nil {
			t.Fatalf("valid CPUs %q: %v", cpus, err)
		}
	}
	for _, cpus := range []string{"", "-1", "0,", "3-1", "0-1-2", "all", "0;id", "4294967296"} {
		req := request(t)
		req.CPUs = cpus
		if err := req.validate(); err == nil {
			t.Errorf("invalid CPUs %q accepted", cpus)
		}
	}
	for _, change := range []func(*Request){
		func(r *Request) { r.Image = "" },
		func(r *Request) { r.OutputDir = "" },
		func(r *Request) { r.MemoryMiB = 0 },
		func(r *Request) { r.MemoryMiB = -1 },
		func(r *Request) { r.MemoryMiB = math.MaxInt64 },
	} {
		req := request(t)
		change(&req)
		p := fakePodman{}
		if err := run(context.Background(), req, p.command); err == nil || len(p.calls) != 0 {
			t.Errorf("invalid request reached Podman: %+v, %v", req, err)
		}
	}
}

func TestHostRequirementsFailClosed(t *testing.T) {
	for name, info := range map[string]string{
		"missing fields": "{}",
		"malformed":      "not JSON",
		"rootful":        strings.Replace(readyHost, `"rootless":true`, `"rootless":false`, 1),
		"remote":         strings.Replace(readyHost, `"os":"linux"`, `"os":"linux","serviceIsRemote":true`, 1),
		"not Linux":      strings.Replace(readyHost, `"linux"`, `"darwin"`, 1),
		"cgroup v1":      strings.Replace(readyHost, `"v2"`, `"v1"`, 1),
		"no cpuset":      strings.Replace(readyHost, `"cpuset",`, "", 1),
		"no memory":      strings.Replace(readyHost, `,"memory"`, "", 1),
	} {
		t.Run(name, func(t *testing.T) {
			req := request(t)
			p := fakePodman{info: info}
			if err := run(context.Background(), req, p.command); err == nil {
				t.Fatal("unsupported host accepted")
			}
			if len(p.calls) != 1 {
				t.Fatalf("continued past failed preflight: %v", p.calls)
			}
		})
	}
}

func TestMalformedPodmanResponses(t *testing.T) {
	for _, image := range []string{"[]", "not JSON", `[{"Id":"mutable-tag"}]`} {
		req := request(t)
		p := fakePodman{image: image}
		if err := run(context.Background(), req, p.command); err == nil || p.called("run") {
			t.Fatalf("invalid image inspection accepted: %s", image)
		}
	}
	for _, mutate := range []func(string) string{
		func(string) string { return "[]" },
		func(string) string { return "not JSON" },
		func(s string) string { return strings.Replace(s, `"exited"`, `"running"`, 1) },
		func(s string) string { return strings.Replace(s, `"ExitCode":0,`, "", 1) },
		func(s string) string { return strings.Replace(s, `"ExitCode":0`, `"ExitCode":-1`, 1) },
		func(s string) string { return strings.Replace(s, `"ExitCode":0`, `"ExitCode":256`, 1) },
	} {
		req := request(t)
		p := fakePodman{}
		p.onCall = func(args []string) {
			if args[0] == "container" {
				p.inspection = mutate(p.containerJSON("exited"))
			}
		}
		if err := run(context.Background(), req, p.command); err == nil || p.called("rm") {
			t.Fatalf("invalid container inspection accepted: %s", p.inspection)
		}
	}
}

func TestPodmanIntegration(t *testing.T) {
	image := os.Getenv("ORDO_TEST_IMAGE")
	cpus := os.Getenv("ORDO_TEST_CPUS")
	if image == "" || cpus == "" {
		t.Skip("set ORDO_TEST_IMAGE and ORDO_TEST_CPUS to run a local nix2container image")
	}
	for _, mode := range []string{"run", "recover"} {
		t.Run(mode, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "ordo-integration-")
			if err != nil {
				t.Fatal(err)
			}
			req := Request{Image: image, CPUs: cpus, MemoryMiB: 128, OutputDir: filepath.Join(dir, "run")}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if mode == "recover" {
				interrupted := func(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
					if args[0] == "wait" {
						return context.Canceled
					}
					return execPodman(ctx, stdout, stderr, args...)
				}
				if err := run(ctx, req, interrupted); !errors.Is(err, context.Canceled) {
					t.Fatalf("expected interruption, got %v; retained results at %s", err, req.OutputDir)
				}
				err = Recover(ctx, req.OutputDir)
			} else {
				err = Run(ctx, req)
			}
			if err != nil {
				t.Fatalf("%v; retained results at %s (container may still be running)", err, req.OutputDir)
			}
			if r := readRecord(t, req.OutputDir); !r.ContainerRemoved || r.ExitCode == nil || *r.ExitCode != 0 {
				t.Fatalf("unexpected result at %s: %+v", req.OutputDir, r)
			}
			if err := os.RemoveAll(dir); err != nil {
				t.Fatal(err)
			}
		})
	}
}
