// Package pool allocates CPUs, memory and GPUs from a worker's configured hardware.
package pool

import (
	"errors"

	"github.com/r3clab/Ordo/internal/spec"
)

var ErrCapacity = errors.New("worker has insufficient unreserved resources")

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

func Plan(h Hardware, req spec.Resources, held []Allocation) (Allocation, error) {
	used := map[int]bool{}
	gpuUsed := map[string]bool{}
	remaining := h.MemoryMiB
	for _, a := range held {
		if a.MemoryMiB < 0 || a.MemoryMiB > remaining {
			return Allocation{}, errors.New("reservations exceed the memory budget")
		}
		remaining -= a.MemoryMiB
		for _, id := range a.GPUs {
			gpuUsed[id] = true
		}
		for _, cpu := range a.CPUs {
			used[cpu] = true
		}
	}
	if req.MemoryMiB > remaining {
		return Allocation{}, ErrCapacity
	}
	a := Allocation{MemoryMiB: req.MemoryMiB}
	for _, cpu := range h.CPUs {
		if len(a.CPUs) == req.Cores {
			break
		}
		if !used[cpu] {
			a.CPUs = append(a.CPUs, cpu)
		}
	}
	if len(a.CPUs) != req.Cores {
		return Allocation{}, ErrCapacity
	}
	for _, gpu := range h.GPUs {
		if len(a.GPUs) == req.GPUs {
			break
		}
		if !gpuUsed[gpu.UUID] {
			a.GPUs = append(a.GPUs, gpu.UUID)
		}
	}
	if len(a.GPUs) != req.GPUs {
		return Allocation{}, ErrCapacity
	}
	return a, nil
}
