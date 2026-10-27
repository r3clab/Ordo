package pool

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/spec"
)

func TestAllocationRespectsHeld(t *testing.T) {
	gpu := "GPU-11111111-1111-1111-1111-111111111111"
	h := Hardware{CPUs: []int{0, 64}, MemoryMiB: 512, GPUs: []GPU{{UUID: gpu}}}
	req := spec.Resources{Cores: 1, MemoryMiB: 256, GPUs: 1}
	first, err := Plan(h, req, nil)
	if err != nil || !slices.Equal(first.CPUs, []int{0}) || first.GPUs[0] != gpu {
		t.Fatal("first allocation", first, err)
	}
	if _, err := Plan(h, req, []Allocation{first}); !errors.Is(err, ErrCapacity) || !strings.HasSuffix(err.Error(), "GPUs") {
		t.Fatal("overlapping GPU admitted", err)
	}
	req.GPUs = 0
	second, err := Plan(h, req, []Allocation{first})
	if err != nil || !slices.Equal(second.CPUs, []int{64}) {
		t.Fatal("second allocation", second, err)
	}
	if _, err := Plan(h, spec.Resources{Cores: 1, MemoryMiB: 1}, []Allocation{first, second}); !errors.Is(err, ErrCapacity) {
		t.Fatal("exceeded the memory budget or reused a core", err)
	}
	if _, err := Plan(h, spec.Resources{Cores: 1, MemoryMiB: 1}, []Allocation{{MemoryMiB: 1024}}); err == nil || errors.Is(err, ErrCapacity) {
		t.Fatal("reservations beyond the budget must be an error, not a capacity wait", err)
	}
}
