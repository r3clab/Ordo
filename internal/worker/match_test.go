package worker

import (
	"errors"
	"slices"
	"testing"
)

func TestMatchingFiltersAllocatedDevices(t *testing.T) {
	h := Hardware{Arch: "amd64", CPUModel: "CPU", Cores: [][]int{{0, 64}}, MemoryMiB: 512, GPUs: []GPU{{UUID: "small", Model: "small", MemoryMiB: 100, Driver: "old"}, {UUID: "large", Model: "large", MemoryMiB: 1000, Driver: "new"}}}
	spec := Spec{Resources: Resources{Cores: 1, MemoryMiB: 128, GPUs: 1}, Match: &Match{Arch: "amd64", CPUModel: "CPU", GPUModel: "large", MinGPUMemoryMiB: 500, GPUDriver: "new"}}
	if !FitsSpec(h, spec, nil) {
		t.Fatal("matching worker rejected")
	}
	filtered, ok := matchedHardware(h, spec.Match)
	allocation, err := allocate(filtered, spec.Resources, nil)
	if !ok || err != nil || len(allocation.GPUs) != 1 || allocation.GPUs[0] != "large" {
		t.Fatal("allocated mismatched GPU", err)
	}
	spec.Match.Arch = "arm64"
	if FitsSpec(h, spec, nil) {
		t.Fatal("architecture mismatch accepted")
	}
	spec.Match.Arch = "amd64"
	spec.Match.GPUDriver = "missing"
	if FitsSpec(h, spec, nil) {
		t.Fatal("driver mismatch accepted")
	}
	if err := spec.Match.Validate(Resources{Cores: 1, MemoryMiB: 128}); err == nil {
		t.Fatal("GPU matching without GPU request accepted")
	}
}
func TestAgentEnforcesMatching(t *testing.T) {
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	spec := testSpec()
	spec.Match = &Match{Arch: "arm64"}
	if _, err := a.Submit("mismatch", spec, nil); !errors.Is(err, ErrCapacity) {
		t.Fatal("direct client bypassed matching", err)
	}
}

func TestNormalizeSortsMatchTags(t *testing.T) {
	m := Spec{Match: &Match{Tags: []string{"l40s", "fast"}}}.Normalize().Match
	if !slices.Equal(m.Tags, []string{"fast", "l40s"}) {
		t.Fatal("tag order changes job identity", m.Tags)
	}
}
