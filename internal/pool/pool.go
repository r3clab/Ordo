// Package pool allocates whole physical cores, memory and GPUs from a worker's configured hardware.
package pool

import (
	"errors"
	"slices"

	"github.com/r3clab/Ordo/internal/spec"
)

var ErrCapacity = errors.New("worker has insufficient unreserved resources")

type Hardware struct {
	Hostname  string  `json:"hostname"`
	MachineID string  `json:"machine_id"`
	Arch      string  `json:"arch"`
	CPUModel  string  `json:"cpu_model"`
	Cores     [][]int `json:"cores"`
	MemoryMiB int64   `json:"memory_mib"`
	GPUs      []GPU   `json:"gpus,omitempty"`
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

// Plan takes whole physical cores, so a task never shares an SMT sibling with another task.
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
	count := 0
	for _, core := range h.Cores {
		if slices.ContainsFunc(core, func(cpu int) bool { return used[cpu] }) {
			continue
		}
		a.CPUs = append(a.CPUs, core...)
		count++
		if count == req.Cores {
			break
		}
	}
	if count != req.Cores {
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
