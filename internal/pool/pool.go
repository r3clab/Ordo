// Package pool allocates CPUs, memory and GPUs from a worker's configured hardware.
package pool

import (
	"errors"
	"fmt"

	"github.com/r3clab/Ordo/internal/spec"
)

var ErrCapacity = errors.New("insufficient unreserved")

type Hardware struct {
	Hostname  string `json:"hostname"`
	MachineID string `json:"machine_id"`
	Arch      string `json:"arch"`
	CPUModel  string `json:"cpu_model"`
	CPUs      []int  `json:"cpus"`
	MemoryMiB int64  `json:"memory_mib"`
	GPUs      []GPU  `json:"gpus,omitempty"`
}

type GPU struct {
	UUID      string `json:"uuid"`
	Index     int    `json:"index"`
	Model     string `json:"model"`
	MemoryMiB int64  `json:"memory_mib"`
}

type Allocation struct {
	CPUs      []int    `json:"cpus"`
	MemoryMiB int64    `json:"memory_mib"`
	GPUs      []string `json:"gpus,omitempty"`
}

// Planner accounts for reservations within one scheduling pass.
// Plan only proposes an allocation; Reserve claims it after the caller commits it.
type Planner struct {
	hardware  Hardware
	used      map[int]bool
	gpuUsed   map[string]bool
	remaining int64
}

func New(h Hardware, held []Allocation) (*Planner, error) {
	p := &Planner{hardware: h, used: map[int]bool{}, gpuUsed: map[string]bool{}, remaining: h.MemoryMiB}
	for _, a := range held {
		if err := p.Reserve(a); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// Reserve also accepts GPU-only reservations for devices busy outside Ordo.
func (p *Planner) Reserve(a Allocation) error {
	if a.MemoryMiB < 0 {
		return errors.New("negative memory reservation")
	}
	p.remaining -= a.MemoryMiB
	for _, id := range a.GPUs {
		p.gpuUsed[id] = true
	}
	for _, cpu := range a.CPUs {
		p.used[cpu] = true
	}
	return nil
}

func (p *Planner) Plan(req spec.Resources) (Allocation, error) {
	// A reload can shrink the budget below what running jobs hold.
	if req.MemoryMiB > p.remaining {
		return Allocation{}, fmt.Errorf("%w memory", ErrCapacity)
	}
	a := Allocation{MemoryMiB: req.MemoryMiB}
	for _, cpu := range p.hardware.CPUs {
		if len(a.CPUs) == req.Cores {
			break
		}
		if !p.used[cpu] {
			a.CPUs = append(a.CPUs, cpu)
		}
	}
	if len(a.CPUs) != req.Cores {
		return Allocation{}, fmt.Errorf("%w cores", ErrCapacity)
	}
	for _, gpu := range p.hardware.GPUs {
		if len(a.GPUs) == req.GPUs {
			break
		}
		if !p.gpuUsed[gpu.UUID] {
			a.GPUs = append(a.GPUs, gpu.UUID)
		}
	}
	if len(a.GPUs) != req.GPUs {
		return Allocation{}, fmt.Errorf("%w GPUs", ErrCapacity)
	}
	return a, nil
}
