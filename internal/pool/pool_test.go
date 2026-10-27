package pool

import (
	"errors"
	"slices"
	"testing"

	"github.com/r3clab/Ordo/internal/spec"
)

func TestMatchingFiltersAllocatedDevices(t *testing.T) {
	h := Hardware{Arch: "amd64", CPUModel: "CPU", Cores: [][]int{{0, 64}}, MemoryMiB: 512, GPUs: []GPU{{UUID: "small", Model: "small", MemoryMiB: 100, Driver: "old"}, {UUID: "large", Model: "large", MemoryMiB: 1000, Driver: "new"}}}
	s := spec.Spec{Resources: spec.Resources{Cores: 1, MemoryMiB: 128, GPUs: 1}, Match: &spec.Match{Arch: "amd64", CPUModel: "CPU", GPUModel: "large", MinGPUMemoryMiB: 500, GPUDriver: "new"}}
	allocation, err := Plan(h, s, nil)
	if err != nil || len(allocation.GPUs) != 1 || allocation.GPUs[0] != "large" {
		t.Fatal("allocated mismatched GPU", err)
	}
	s.Match.Arch = "arm64"
	if _, err := Plan(h, s, nil); !errors.Is(err, ErrCapacity) {
		t.Fatal("architecture mismatch accepted")
	}
	s.Match.Arch = "amd64"
	s.Match.GPUDriver = "missing"
	if _, err := Plan(h, s, nil); !errors.Is(err, ErrCapacity) {
		t.Fatal("driver mismatch accepted")
	}
}

func TestAllocationTakesWholeCoresAndRespectsHeld(t *testing.T) {
	gpu := "GPU-11111111-1111-1111-1111-111111111111"
	h := Hardware{Cores: [][]int{{0, 64}, {1, 65}}, MemoryMiB: 512, GPUs: []GPU{{UUID: gpu}}}
	req := spec.Resources{Cores: 1, MemoryMiB: 256, GPUs: 1}
	first, err := allocate(h, req, nil)
	if err != nil || !slices.Equal(first.CPUs, []int{0, 64}) || first.GPUs[0] != gpu {
		t.Fatal("first allocation", first, err)
	}
	if _, err := allocate(h, req, []Allocation{first}); !errors.Is(err, ErrCapacity) {
		t.Fatal("overlapping GPU admitted", err)
	}
	req.GPUs = 0
	second, err := allocate(h, req, []Allocation{first})
	if err != nil || !slices.Equal(second.CPUs, []int{1, 65}) {
		t.Fatal("second allocation", second, err)
	}
	if _, err := allocate(h, spec.Resources{Cores: 1, MemoryMiB: 1}, []Allocation{first, second}); !errors.Is(err, ErrCapacity) {
		t.Fatal("exceeded the memory budget or reused a core", err)
	}
	if _, err := allocate(h, spec.Resources{Cores: 1, MemoryMiB: 1}, []Allocation{{MemoryMiB: 1024}}); err == nil || errors.Is(err, ErrCapacity) {
		t.Fatal("reservations beyond the budget must be an error, not a capacity wait", err)
	}
}
