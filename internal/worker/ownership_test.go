package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHostOwnershipSurvivesStoppedAgentWithUnfinishedWork(t *testing.T) {
	runtimeDir := t.TempDir()
	if err := os.Chmod(runtimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a, cfg, h := testAgent(t)
	owner, err := lockHost(runtimeDir, cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := bindHost(owner, cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	a.owner = owner
	if other, err := lockHost(runtimeDir, filepath.Join(t.TempDir(), "other")); err == nil {
		_ = other.Close()
		t.Fatal("allowed a concurrent agent")
	}
	if _, err := a.Submit("held", testSpec(), nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := lockHost(runtimeDir, "/different"); err == nil {
		_ = other.Close()
		t.Fatal("changed state directory while work was unfinished")
	}
	owner, err = lockHost(runtimeDir, cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	a, err = open(context.Background(), cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	a.owner = owner
	if err := bindHost(owner, cfg.StateDir); err != nil {
		t.Fatal(err)
	}
	run := a.attempts["held"]
	run.State = "cancelled"
	if err := a.save(run); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err = lockHost(runtimeDir, "/different")
	if err != nil {
		t.Fatal(err)
	}
	if err := releaseHost(owner, true); err != nil {
		t.Fatal(err)
	}
}

func TestHostOwnershipRejectsCorruptionAndSymlinks(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(dir, "ordo-worker.lock")
		if symlink {
			if err := os.Symlink(filepath.Join(t.TempDir(), "target"), file); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.WriteFile(file, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if owner, err := lockHost(dir, "/state"); err == nil {
			_ = owner.Close()
			t.Fatal("accepted unsafe ownership record")
		}
	}
}
