package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSecretsDropsOneTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{}
	for name, content := range map[string]string{"ECHOED": "x\n", "BARE": "y", "DOUBLE": "z\n\n"} {
		files[name] = filepath.Join(dir, name)
		if err := os.WriteFile(files[name], []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	values, err := readSecrets(files)
	if err != nil || values["ECHOED"] != "x" || values["BARE"] != "y" || values["DOUBLE"] != "z\n" {
		t.Fatalf("%q %v", values, err)
	}
	if _, err := readSecrets(map[string]string{"MISSING": filepath.Join(dir, "missing")}); err == nil {
		t.Fatal("read a missing secret file")
	}
}
