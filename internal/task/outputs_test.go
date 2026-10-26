package task

import (
	"github.com/r3clab/Ordo/internal/content"
	"os"
	"path/filepath"
	"testing"
)

func TestSavedOutputArchiveRequired(t *testing.T) {
	dir := t.TempDir()
	r := record{Request: Request{OutputDir: dir, ExportOutputs: true}, ExitCode: new(0)}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := savedResults(&r); err == nil {
		t.Fatal("accepted missing outputs")
	}
	empty := t.TempDir()
	hash, err := content.Archive(empty, filepath.Join(dir, "outputs.tar"))
	if err != nil {
		t.Fatal(err)
	}
	r.OutputsSHA256 = hash
	if err := savedResults(&r); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "outputs.tar"), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := savedResults(&r); err == nil {
		t.Fatal("accepted corrupted outputs")
	}
}
