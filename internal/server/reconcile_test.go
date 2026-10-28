package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/worker"
)

type gpuHost struct {
	*fakeHost
	gpus    []pool.GPU
	busy    []string
	gpuErr  error
	samples int
}

func (h *gpuHost) Probe(ctx context.Context) (worker.Probe, error) {
	p, err := h.fakeHost.Probe(ctx)
	p.Hardware.GPUs = h.gpus
	return p, err
}

func (h *gpuHost) GPUBusy(context.Context, []pool.GPU) ([]string, error) {
	h.samples++
	return h.busy, h.gpuErr
}

func TestPassReservesCommittedAllocationsAndSamplesGPUsOnce(t *testing.T) {
	cfg := testWorker("worker")
	cfg.GPUs = []string{
		"GPU-11111111-1111-1111-1111-111111111111",
		"GPU-22222222-2222-2222-2222-222222222222",
		"GPU-33333333-3333-3333-3333-333333333333",
	}
	s := open(t, filepath.Join(t.TempDir(), "state"), []worker.Config{cfg}, nil)
	defer func() { _ = s.Close() }()
	h := &gpuHost{fakeHost: newFakeHost(s.id), busy: cfg.GPUs[:1], gpuErr: errors.New("GPU sample unavailable")}
	for _, id := range cfg.GPUs {
		h.gpus = append(h.gpus, pool.GPU{UUID: id})
	}
	out := t.TempDir()
	cpu := submit(t, s, jobRequest(filepath.Join(out, "cpu")))
	for _, id := range cfg.GPUs {
		r := jobRequest(filepath.Join(out, id))
		r.Resources.GPUs = 1
		submit(t, s, r)
	}
	mustTick(t, s, cfg, h)
	if len(h.launches) != 1 || h.samples != 3 || mustJob(t, s, cpu).State != Running {
		t.Fatalf("GPU sample errors must not block CPU jobs or be cached: launches=%d, samples=%d", len(h.launches), h.samples)
	}
	h.gpuErr = nil
	mustTick(t, s, cfg, h)
	if len(h.launches) != 3 || h.samples != 4 {
		t.Fatalf("one sample must serve the whole pass: launches=%d, samples=%d", len(h.launches), h.samples)
	}
	for _, l := range h.launches {
		for _, id := range l.Allocation.GPUs {
			if id == cfg.GPUs[0] {
				t.Fatal("allocated an externally busy GPU")
			}
		}
	}
	mustTick(t, s, cfg, h)
	if len(h.launches) != 3 || h.samples != 5 {
		t.Fatalf("next pass must retain reservations and refresh the sample: launches=%d, samples=%d", len(h.launches), h.samples)
	}
	h.busy = nil
	mustTick(t, s, cfg, h)
	if len(h.launches) != 4 || h.samples != 6 {
		t.Fatalf("newly idle GPU must become available next pass: launches=%d, samples=%d", len(h.launches), h.samples)
	}
	cpus, gpus := map[int]bool{}, map[string]bool{}
	for _, l := range h.launches {
		for _, cpu := range l.Allocation.CPUs {
			if cpus[cpu] {
				t.Fatal("overlapping CPU reservations", cpu)
			}
			cpus[cpu] = true
		}
		for _, id := range l.Allocation.GPUs {
			if gpus[id] {
				t.Fatal("overlapping GPU reservations", id)
			}
			gpus[id] = true
		}
	}
}
