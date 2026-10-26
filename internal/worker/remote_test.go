package worker

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRemoteValidationAndShellQuoting(t *testing.T) {
	good := Remote{Host: "vegeta", StateDir: "/tmp/ordo-worker", CPUs: "62,63,126,127", MemoryMiB: 512}
	if err := good.validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Remote){func(r *Remote) { r.Host = "-oProxyCommand=bad" }, func(r *Remote) { r.StateDir = "/tmp/../etc" }, func(r *Remote) { r.StateDir = "/tmp/a:b" }, func(r *Remote) { r.CPUs = "62,62" }, func(r *Remote) { r.MemoryMiB = 0 }} {
		bad := good
		change(&bad)
		if err := bad.validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	if shellQuote("a'b $x") != "'a'\\''b $x'" {
		t.Fatal("unsafe quoting")
	}
	if unitName(good.StateDir) != "ordo-agent-ea98d0ead75e656b" || unitName(good.StateDir) == unitName("/tmp/other") {
		t.Fatal("unstable unit identity")
	}
	if _, err := runSSH(context.Background(), "--bad", "", nil); err == nil {
		t.Fatal("allowed SSH option injection")
	}
	if _, _, err := artifact(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted missing artifact")
	}
}
