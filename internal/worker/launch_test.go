package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("ORDO_TEST_FAILED_LAUNCH") == "1" {
		switch os.Args[2] {
		case "info":
			fmt.Println(`{"Host":{"OS":"linux","CgroupVersion":"v2","CgroupControllers":["cpuset","memory"],"Security":{"Rootless":true}}}`)
		case "image":
			fmt.Printf(`[{"ID":"sha256:%s"}]`, strings.Repeat("a", 64))
		case "run":
			os.Exit(125)
		case "container":
			os.Exit(1)
		default:
			os.Exit(99)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestFailedLaunchWithoutSavedIdentityKeepsReservation(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(binary, filepath.Join(dir, "podman")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ORDO_TEST_FAILED_LAUNCH", "1")
	t.Setenv("INVOCATION_ID", "")
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	run, err := a.Submit("ambiguous", testSpec())
	if err != nil {
		t.Fatal(err)
	}
	a.wg.Add(1)
	a.execute(run, true)
	if got := a.attempts[run.ID]; got.State != "blocked" || got.Result == nil || got.Result.ImageID == "" || got.Result.ContainerID != "" {
		t.Fatalf("lost ambiguous reservation: %+v", got)
	}
}
