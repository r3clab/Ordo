package spec

import (
	"slices"
	"strings"
	"testing"
)

func TestSandboxValidation(t *testing.T) {
	type sandbox struct {
		Sandbox
		secrets map[string]string
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
		"secret is env":   {Sandbox: Sandbox{Env: map[string]string{"TOKEN": ""}}, secrets: map[string]string{"TOKEN": ""}},
		"reserved secret": {secrets: map[string]string{"NVIDIA_DRIVER_CAPABILITIES": ""}},
		"secret newline":  {secrets: map[string]string{"TOKEN": "a\nb"}},
		"secret return":   {secrets: map[string]string{"TOKEN": "a\rb"}},
		"secret NUL":      {secrets: map[string]string{"TOKEN": "a\x00b"}},
		"secret size":     {secrets: map[string]string{"TOKEN": large}},
	} {
		if err := c.Validate(c.secrets); err == nil {
			t.Fatalf("%s: accepted invalid sandbox", name)
		}
	}
	if err := (Sandbox{Network: "none", CapAdd: []string{"NET_ADMIN"}, Env: map[string]string{"A": "x=y"}}).Validate(map[string]string{"TOKEN": "a b=c", "OTHER": ""}); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeSortsMatchTags(t *testing.T) {
	m := Spec{Match: &Match{Tags: []string{"l40s", "fast"}}}.Normalize().Match
	if !slices.Equal(m.Tags, []string{"fast", "l40s"}) {
		t.Fatal("tag order changes job identity", m.Tags)
	}
}

func TestMatchValidation(t *testing.T) {
	if err := (Match{GPUModel: "L40S"}).Validate(Resources{Cores: 1, MemoryMiB: 128}); err == nil {
		t.Fatal("GPU matching without GPU request accepted")
	}
	if err := (Match{Arch: "riscv64"}).Validate(Resources{Cores: 1, MemoryMiB: 128}); err == nil {
		t.Fatal("unknown architecture accepted")
	}
}

func TestGPUIdentifiers(t *testing.T) {
	if !ValidGPUSet([]string{"GPU-11111111-1111-1111-1111-111111111111"}) {
		t.Fatal("rejected a full GPU UUID")
	}
	for _, ids := range [][]string{{"all"}, {"0"}, {"MIG-example"}, {"GPU-11111111-1111-1111-1111-111111111111", "GPU-11111111-1111-1111-1111-111111111111"}} {
		if ValidGPUSet(ids) {
			t.Fatal("unsafe GPU pool", ids)
		}
	}
}
