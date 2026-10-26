package worker

import (
	"context"
	"errors"
	"testing"
)

func TestGPUReservationsAndRestoration(t *testing.T) {
	a, cfg, h := testAgent(t)
	gpu := GPU{UUID: "GPU-11111111-1111-1111-1111-111111111111", Model: "test", MemoryMiB: 1024, Driver: "580"}
	h.GPUs = []GPU{gpu}
	req := Resources{Cores: 1, MemoryMiB: 128, GPUs: 1}
	allocation, err := allocate(h, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allocate(h, req, []Allocation{allocation}); !errors.Is(err, ErrCapacity) {
		t.Fatal("overlapping GPU admitted", err)
	}
	run := Attempt{ID: "gpu", State: "running", Hardware: h, Spec: testSpec(), Allocation: allocation}
	run.Spec.Resources = req
	if err := a.save(run); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = open(context.Background(), cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	if a.attempts["gpu"].Allocation.GPUs[0] != gpu.UUID {
		t.Fatal("reservation lost")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	h.GPUs[0].Driver = "changed"
	if other, err := open(context.Background(), cfg, h); err == nil {
		_ = other.Close()
		t.Fatal("changed driver accepted with active GPU reservation")
	}
}
func TestGPUIdentifiers(t *testing.T) {
	for _, text := range []string{"all", "0", "MIG-example", "GPU-11111111-1111-1111-1111-111111111111,GPU-11111111-1111-1111-1111-111111111111"} {
		if _, err := GPUList(text); err == nil {
			t.Fatal("unsafe GPU pool", text)
		}
	}
}
