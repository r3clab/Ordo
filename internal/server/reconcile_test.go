package server

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/worker"
)

func TestPassLeavesOtherWorkersActiveJobsAlone(t *testing.T) {
	for _, state := range []State{Dispatching, Running, Collecting, Queued, Blocked, Delivering} {
		t.Run(string(state), func(t *testing.T) {
			f := newFixture(t)
			id := f.submit(t, jobRequest(f.out("job")))
			f.tick(t)
			if state == Delivering {
				running := f.job(t, id)
				f.h.exit(running.current().ID, 0)
				f.tick(t)
			}
			j := f.job(t, id)
			j.State = state
			j.CancelRequested = state == Blocked
			if state == Queued {
				f.h.exit(j.current().ID, 1)
				j.current().Failure = InfrastructureFailure
			}
			seed(t, f.s, j)
			other := testWorker("other")
			h := newFakeHost(f.s.id)
			mustTick(t, f.s, other, h)
			got := f.job(t, id)
			switch state {
			case Dispatching, Running, Collecting:
				if !reflect.DeepEqual(got, j) {
					t.Fatalf("another worker changed an active job: %+v", got)
				}
			case Queued:
				if got.State != Running || len(got.Attempts) != 2 || got.current().Worker != other.Host || len(h.launches) != 1 {
					t.Fatalf("previous worker prevented retry placement: %+v", got)
				}
			case Blocked:
				if got.State != Cancelled {
					t.Fatalf("another worker must still process blocked cancellation: %+v", got)
				}
			case Delivering:
				if got.State != Succeeded || readOut(t, got.Out, "score") != j.current().ID {
					t.Fatalf("another worker must still deliver collected results: %+v", got)
				}
			}
			if state != Queued && len(h.launches) != 0 || h.collects != 0 || len(h.stops) != 0 {
				t.Fatal("another worker operated on the attempt")
			}
		})
	}
}

func BenchmarkPassOtherWorkersJobs(b *testing.B) {
	s := &Server{jobs: map[string]Job{}, busy: map[string]bool{}, nodes: map[string]WorkerStatus{}}
	for i := range 1000 {
		id := strconv.Itoa(i)
		s.jobs[id] = Job{ID: id, State: Running, CreatedAt: time.Unix(int64(i), 0), Attempts: []Attempt{{Worker: "other"}}}
	}
	cfg := testWorker("worker")
	h := newFakeHost(s.id)
	ctx := context.Background()
	probe, err := h.Probe(ctx)
	if err != nil {
		b.Fatal(err)
	}
	bg := &steps{}
	b.ReportAllocs()
	for b.Loop() {
		if err := s.pass(ctx, cfg, h, &probe, bg); err != nil {
			b.Fatal(err)
		}
	}
}

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
