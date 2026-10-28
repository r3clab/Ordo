package pool

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/spec"
)

func BenchmarkPlanningPass(b *testing.B) {
	h := Hardware{CPUs: make([]int, 256), MemoryMiB: 32768}
	for i := range h.CPUs {
		h.CPUs[i] = i
	}
	req := spec.Resources{Cores: 1, MemoryMiB: 128}
	b.ReportAllocs()
	for b.Loop() {
		p, err := New(h, nil)
		if err != nil {
			b.Fatal(err)
		}
		for range len(h.CPUs) {
			a, err := p.Plan(req)
			if err != nil {
				b.Fatal(err)
			}
			if err := p.Reserve(a); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func TestAllocationRespectsHeld(t *testing.T) {
	gpu := "GPU-11111111-1111-1111-1111-111111111111"
	h := Hardware{CPUs: []int{0, 64}, MemoryMiB: 512, GPUs: []GPU{{UUID: gpu}}}
	req := spec.Resources{Cores: 1, MemoryMiB: 256, GPUs: 1}
	p, err := New(h, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.Plan(req)
	if err != nil || !slices.Equal(first.CPUs, []int{0}) || !slices.Equal(first.GPUs, []string{gpu}) {
		t.Fatal("first allocation", first, err)
	}
	if err := p.Reserve(first); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Plan(req); !errors.Is(err, ErrCapacity) || !strings.HasSuffix(err.Error(), "GPUs") {
		t.Fatal("overlapping GPU admitted", err)
	}
	req.GPUs = 0
	second, err := p.Plan(req)
	if err != nil || !slices.Equal(second.CPUs, []int{64}) {
		t.Fatal("second allocation", second, err)
	}
	if err := p.Reserve(second); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Plan(spec.Resources{Cores: 1, MemoryMiB: 1}); !errors.Is(err, ErrCapacity) {
		t.Fatal("exceeded the memory budget or reused a core", err)
	}
	p, err = New(h, []Allocation{{MemoryMiB: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Plan(spec.Resources{Cores: 1, MemoryMiB: 1}); !errors.Is(err, ErrCapacity) {
		t.Fatal("reservations beyond a shrunk budget must wait for capacity", err)
	}
	if _, err := New(h, []Allocation{{MemoryMiB: -1}}); err == nil || errors.Is(err, ErrCapacity) {
		t.Fatal("a negative reservation must be an error, not a capacity wait", err)
	}
}

func TestPlanningDoesNotReserveUntilCommitted(t *testing.T) {
	h := Hardware{CPUs: []int{0, 64}, MemoryMiB: 512, GPUs: []GPU{{UUID: "busy"}, {UUID: "free"}}}
	p, err := New(h, []Allocation{{CPUs: []int{0}, MemoryMiB: 256}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve(Allocation{GPUs: []string{"busy"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Reserve(Allocation{CPUs: []int{64}, GPUs: []string{"free"}, MemoryMiB: -1}); err == nil {
		t.Fatal("negative reservation accepted")
	}
	for _, req := range []spec.Resources{
		{Cores: 1, MemoryMiB: 257},
		{Cores: 2, MemoryMiB: 128},
		{Cores: 1, MemoryMiB: 128, GPUs: 2},
	} {
		if _, err := p.Plan(req); !errors.Is(err, ErrCapacity) {
			t.Fatalf("expected capacity failure for %+v: %v", req, err)
		}
	}
	for range 2 {
		a, err := p.Plan(spec.Resources{Cores: 1, MemoryMiB: 256, GPUs: 1})
		if err != nil || !slices.Equal(a.CPUs, []int{64}) || !slices.Equal(a.GPUs, []string{"free"}) || a.MemoryMiB != 256 {
			t.Fatalf("planning or a failed reservation changed capacity: %+v, %v", a, err)
		}
	}
}
