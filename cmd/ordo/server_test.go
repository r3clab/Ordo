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

func TestStateAndConfigDefaultToOrdoEnvThenXDG(t *testing.T) {
	t.Setenv("HOME", "/home/me")
	t.Setenv("ORDO_STATE", "/srv/ordo")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	if got := defaultState(); got != "/srv/ordo" {
		t.Fatalf("ORDO_STATE ignored: %q", got)
	}
	t.Setenv("ORDO_STATE", "")
	if got := defaultState(); got != "/xdg/state/ordo/server" {
		t.Fatalf("XDG_STATE_HOME ignored: %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "relative")
	if got := defaultState(); got != "/home/me/.local/state/ordo/server" {
		t.Fatalf("a relative XDG_STATE_HOME must be ignored: %q", got)
	}
	t.Setenv("ORDO_CONFIG", "/etc/ordo/server.json")
	t.Setenv("XDG_CONFIG_HOME", "/xdg/config")
	if got := defaultConfig(); got != "/etc/ordo/server.json" {
		t.Fatalf("ORDO_CONFIG ignored: %q", got)
	}
	t.Setenv("ORDO_CONFIG", "")
	if got := defaultConfig(); got != "/xdg/config/ordo/server.json" {
		t.Fatalf("XDG_CONFIG_HOME ignored: %q", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if got := defaultConfig(); got != "/home/me/.config/ordo/server.json" {
		t.Fatalf("config default %q", got)
	}
}

func TestServerConfigDefaultsAndResolvesSecrets(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "server.json")
	write := func(content string) {
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"workers":[{"host":"vegeta","cpus":"0-3","memory_mib":512}],"secrets":{"TOKEN":"token","ABS":"/run/abs"}}`)
	c, err := loadServerConfig(file)
	if err != nil || c.Listen != "127.0.0.1:7331" || len(c.Workers) != 1 || c.Workers[0].StateDir != "" {
		t.Fatalf("%+v %v", c, err)
	}
	if c.Secrets["TOKEN"] != filepath.Join(dir, "token") || c.Secrets["ABS"] != "/run/abs" {
		t.Fatalf("secret paths %q", c.Secrets)
	}
	write(`{"workers":[],"color":"red"}`)
	if _, err := loadServerConfig(file); err == nil {
		t.Fatal("accepted an unknown key")
	}
}
