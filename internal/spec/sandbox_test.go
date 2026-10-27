package spec

import (
	"slices"
	"strings"
	"testing"
)

func TestSandboxValidation(t *testing.T) {
	large := strings.Repeat("x", MaxValueBytes+1)
	for name, s := range map[string]Task{
		"network":         {Sandbox: Sandbox{Network: "host"}},
		"capability":      {Sandbox: Sandbox{CapAdd: []string{"SYS_ADMIN"}}},
		"duplicate cap":   {Sandbox: Sandbox{CapAdd: []string{"NET_ADMIN", "NET_ADMIN"}}},
		"env name":        {Sandbox: Sandbox{Env: map[string]string{"1X": ""}}},
		"env NUL":         {Sandbox: Sandbox{Env: map[string]string{"X": "\x00"}}},
		"env size":        {Sandbox: Sandbox{Env: map[string]string{"X": large}}},
		"reserved env":    {Sandbox: Sandbox{Env: map[string]string{"NVIDIA_VISIBLE_DEVICES": "all"}}},
		"reserved secret": {Secrets: []string{"NVIDIA_DRIVER_CAPABILITIES"}},
		"secret name":     {Secrets: []string{"1X"}},
		"repeated secret": {Secrets: []string{"TOKEN", "TOKEN"}},
	} {
		s.Resources = Resources{Cores: 1, MemoryMiB: 128}
		if err := s.Validate(); err == nil {
			t.Fatalf("%s: accepted invalid sandbox", name)
		}
	}
	s := Task{Resources: Resources{Cores: 1, MemoryMiB: 128}, Sandbox: Sandbox{Network: "none", CapAdd: []string{"NET_ADMIN"}, Env: map[string]string{"A": "x=y"}}, Secrets: []string{"TOKEN", "OTHER"}}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSecretValues(t *testing.T) {
	for name, value := range map[string]string{"empty": "", "oversize": strings.Repeat("x", MaxSecretBytes+1)} {
		if CheckSecret("TOKEN", value) == nil {
			t.Fatalf("accepted a %s secret", name)
		}
	}
	if CheckSecret("NVIDIA_DRIVER_CAPABILITIES", "x") == nil || CheckSecret("1X", "x") == nil {
		t.Fatal("accepted an invalid secret name")
	}
	for _, value := range []string{"a b=c", "x", "a\nb\r", "a\x00b", strings.Repeat("x", MaxSecretBytes)} {
		if err := CheckSecret("TOKEN", value); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNormalizeSortsTags(t *testing.T) {
	tags := Task{Tags: []string{"l40s", "fast"}}.Normalize().Tags
	if !slices.Equal(tags, []string{"fast", "l40s"}) {
		t.Fatal("tag order changes job identity", tags)
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
