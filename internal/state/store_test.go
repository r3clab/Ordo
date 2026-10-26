package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
)

func TestDurableStateAndExclusiveOwner(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(dir); !errors.Is(err, syscall.EWOULDBLOCK) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("second owner accepted: %v", err)
	}
	data := map[string]any{"state": "running", "cores": float64(2)}
	if err := s.Put("attempt", "id", data); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var got map[string]any
	if err := s.Get("attempt", "id", &got); err != nil {
		t.Fatal(err)
	}
	if got["state"] != "running" || got["cores"] != float64(2) {
		t.Fatalf("lost state: %v", got)
	}
	var sync, version int
	if err := s.db.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if sync != 2 || version != 1 {
		t.Fatalf("unsafe durability/schema: %d %d", sync, version)
	}
}

func TestRejectFutureSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("PRAGMA user_version=2"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir); err == nil {
		_ = s.Close()
		t.Fatal("silently accepted unknown schema")
	}
}

func TestDeleteLeavesNoCopyOnDisk(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if err := s.Put("secret", "kept", map[string]string{"TOKEN": "visible"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("secret", "gone", map[string]string{"TOKEN": "hunter2-deleted"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("secret", "gone"); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.IDs("secret"); err != nil || !slices.Equal(ids, []string{"kept"}) {
		t.Fatalf("ids = %q, %v", ids, err)
	}
	files, err := filepath.Glob(filepath.Join(dir, "state.sqlite*"))
	if err != nil || len(files) == 0 {
		t.Fatal("no database files", err)
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("hunter2-deleted")) {
			t.Fatalf("%s still holds the deleted record", file)
		}
	}
}
