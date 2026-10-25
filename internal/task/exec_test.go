package task

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("ORDO_TEST_PODMAN_PROCESS") == "1" {
		if slices.Equal(os.Args[1:], []string{"--help"}) {
			if os.Getenv("ORDO_TEST_EXPANSION_FLAG") == "yes" {
				fmt.Println("--expand-environment=BOOL")
			}
			os.Exit(0)
		}
		data := map[string]any{"program": filepath.Base(os.Args[0]), "args": os.Args[1:], "invocation": os.Getenv("INVOCATION_ID"), "sentinel": os.Getenv("ORDO_TEST_SENTINEL")}
		if err := json.NewEncoder(os.Stdout).Encode(data); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestPodmanDoesNotInheritAgentServiceOwnership(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, name := range []string{"podman", "systemd-run"} {
		if err := os.Symlink(binary, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("ORDO_TEST_PODMAN_PROCESS", "1")
	t.Setenv("ORDO_TEST_SENTINEL", "preserved")
	for _, mode := range []string{"standalone", "old-systemd", "new-systemd"} {
		t.Run(mode, func(t *testing.T) {
			invocation := "agent-service-invocation"
			if mode == "standalone" {
				invocation = ""
			}
			t.Setenv("INVOCATION_ID", invocation)
			flag := "no"
			if mode == "new-systemd" {
				flag = "yes"
			}
			t.Setenv("ORDO_TEST_EXPANSION_FLAG", flag)
			var stdout, stderr bytes.Buffer
			args := []string{"run", "--", "image", "$HOME ${USER} $(echo not-on-host)"}
			if err := execPodman(context.Background(), &stdout, &stderr, args...); err != nil {
				t.Fatalf("%v: %s", err, &stderr)
			}
			var got struct {
				Program              string
				Args                 []string
				Invocation, Sentinel string
			}
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			want := append([]string{"--remote=false"}, args...)
			if mode != "standalone" {
				if got.Program != "systemd-run" || len(got.Args) < 6 || !strings.HasPrefix(got.Args[5], "--unit=ordo-runtime-") {
					t.Fatalf("missing runtime scope: %+v", got)
				}
				prefix := []string{"--user", "--scope", "--quiet", "--collect", "--property=Delegate=yes", got.Args[5]}
				if mode == "new-systemd" {
					prefix = append(prefix, "--expand-environment=no")
				}
				want = append(append(prefix, "--", "podman"), want...)
			} else if got.Program != "podman" {
				t.Fatalf("standalone unexpectedly requires systemd: %+v", got)
			}
			if got.Invocation != "" || got.Sentinel != "preserved" || !slices.Equal(got.Args, want) {
				t.Fatalf("unsafe Podman process context: %+v; want %v", got, want)
			}
			if os.Getenv("INVOCATION_ID") != invocation {
				t.Fatal("changed the parent process environment")
			}
		})
	}
}
