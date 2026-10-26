package task

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSandboxLaunchKeepsSecretValuesOutOfRecords(t *testing.T) {
	req := request(t)
	service := t.TempDir()
	req.Sandbox = Sandbox{Env: map[string]string{"B": "2", "A": "1"}, Network: Egress, CapAdd: []string{"NET_ADMIN"}}
	req.Secrets = Secrets{"TOKEN": "hunter2"}
	req.Services = map[string]string{"model": service}
	secrets := filepath.Join(req.OutputDir, secretsFile)
	p := fakePodman{}
	p.onCall = func(args []string) {
		if args[0] != "run" {
			return
		}
		for _, want := range [][]string{
			{"--memory-swap", "134217728", "--network=pasta", "--cap-add=NET_ADMIN", "--env", "A=1", "--env", "B=2", "--image-volume=ignore"},
			{"--volume", service + ":/services/model:ro", "--env-file", secrets, "--"},
		} {
			start := slices.Index(args, want[0])
			if start < 0 || !slices.Equal(args[start:start+len(want)], want) {
				t.Fatalf("argv %q lacks %q", args, want)
			}
		}
		if slices.ContainsFunc(args, func(arg string) bool { return strings.Contains(arg, "hunter2") }) {
			t.Fatal("secret value reached argv")
		}
		data, err := os.ReadFile(secrets)
		if err != nil || string(data) != "TOKEN=hunter2\n" {
			t.Fatalf("secrets file = %q, %v", data, err)
		}
		if info, _ := os.Stat(secrets); info.Mode().Perm() != 0o600 {
			t.Fatalf("secrets file mode %v", info.Mode())
		}
	}
	if err := run(context.Background(), req, p.command); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secrets); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("secrets file survived launch")
	}
	data, err := os.ReadFile(filepath.Join(req.OutputDir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hunter2") || !strings.Contains(string(data), `"TOKEN"`) {
		t.Fatalf("run.json must name secrets without values: %s", data)
	}
	if r := readRecord(t, req.OutputDir); !slices.Equal(r.Request.Secrets.Names(), []string{"TOKEN"}) || r.Request.Network != Egress {
		t.Fatalf("record lost the sandbox: %+v", r.Request)
	}
}

func TestSandboxValidation(t *testing.T) {
	type sandbox struct {
		Sandbox
		secrets Secrets
	}
	large := strings.Repeat("x", MaxValueBytes+1)
	for name, c := range map[string]sandbox{
		"network":         {Sandbox: Sandbox{Network: "host"}},
		"capability":      {Sandbox: Sandbox{CapAdd: []string{"SYS_ADMIN"}}},
		"duplicate cap":   {Sandbox: Sandbox{CapAdd: []string{"NET_ADMIN", "NET_ADMIN"}}},
		"env name":        {Sandbox: Sandbox{Env: map[string]string{"1X": ""}}},
		"env NUL":         {Sandbox: Sandbox{Env: map[string]string{"X": "\x00"}}},
		"env size":        {Sandbox: Sandbox{Env: map[string]string{"X": large}}},
		"reserved env":    {Sandbox: Sandbox{Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "all"}}},
		"secret is env":   {Sandbox: Sandbox{Env: map[string]string{"TOKEN": ""}}, secrets: Secrets{"TOKEN": ""}},
		"reserved secret": {secrets: Secrets{"NVIDIA_DRIVER_CAPABILITIES": ""}},
		"secret newline":  {secrets: Secrets{"TOKEN": "a\nb"}},
		"secret return":   {secrets: Secrets{"TOKEN": "a\rb"}},
		"secret NUL":      {secrets: Secrets{"TOKEN": "a\x00b"}},
		"secret size":     {secrets: Secrets{"TOKEN": large}},
	} {
		if err := c.Validate(c.secrets); err == nil {
			t.Fatalf("%s: accepted invalid sandbox", name)
		}
	}
	if err := (Sandbox{Network: "none", CapAdd: []string{"NET_ADMIN"}, Env: map[string]string{"A": "x=y"}}).Validate(Secrets{"TOKEN": "a b=c", "OTHER": ""}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoverRemovesStaleSecrets(t *testing.T) {
	req, p := retainedRun(t, "wait", 0)
	stale := filepath.Join(req.OutputDir, secretsFile)
	if err := os.WriteFile(stale, []byte("TOKEN=hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := recoverRun(context.Background(), req.OutputDir, p.command); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale secrets survived recovery")
	}
}
