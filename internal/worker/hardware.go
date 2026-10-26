package worker

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/r3clab/Ordo/internal/task"
)

type Hardware struct {
	Hostname  string  `json:"hostname"`
	MachineID string  `json:"machine_id"`
	Arch      string  `json:"arch"`
	CPUModel  string  `json:"cpu_model"`
	Cores     [][]int `json:"cores"`
	MemoryMiB int64   `json:"memory_mib"`
	GPUs      []GPU   `json:"gpus,omitempty"`
}

type Resources struct {
	Cores     int   `json:"cores"`
	MemoryMiB int64 `json:"memory_mib"`
	GPUs      int   `json:"gpus,omitempty"`
}

type Allocation struct {
	CPUs      []int    `json:"cpus"`
	MemoryMiB int64    `json:"memory_mib"`
	GPUs      []string `json:"gpus,omitempty"`
}

func discover(cpus string, memoryMiB int64) (Hardware, error) {
	if runtime.GOOS != "linux" {
		return Hardware{}, errors.New("worker execution requires Linux")
	}
	pool, err := task.ParseCPUs(cpus)
	if err != nil {
		return Hardware{}, err
	}
	onlineData, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return Hardware{}, err
	}
	online, err := task.ParseCPUs(strings.TrimSpace(string(onlineData)))
	if err != nil {
		return Hardware{}, err
	}
	h := Hardware{Arch: runtime.GOARCH, MemoryMiB: memoryMiB}
	h.Hostname, err = os.Hostname()
	if err != nil {
		return h, err
	}
	id, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return h, err
	}
	h.MachineID = strings.TrimSpace(string(id))
	if h.MachineID == "" {
		return h, errors.New("worker machine ID is empty")
	}
	seen := map[int]bool{}
	for _, cpu := range pool {
		if seen[cpu] {
			continue
		}
		if !slices.Contains(online, cpu) {
			return h, fmt.Errorf("CPU %d is not online", cpu)
		}
		data, err := os.ReadFile(filepath.Join("/sys/devices/system/cpu", fmt.Sprintf("cpu%d", cpu), "topology/thread_siblings_list"))
		if err != nil {
			return h, err
		}
		siblings, err := task.ParseCPUs(strings.TrimSpace(string(data)))
		if err != nil {
			return h, err
		}
		for _, n := range siblings {
			if !slices.Contains(pool, n) || !slices.Contains(online, n) {
				return h, fmt.Errorf("CPU %d requires all online SMT siblings %s in the pool", cpu, task.FormatCPUs(siblings))
			}
			seen[n] = true
		}
		h.Cores = append(h.Cores, siblings)
	}
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return h, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && (strings.TrimSpace(key) == "model name" || strings.TrimSpace(key) == "CPU implementer") {
			h.CPUModel = strings.TrimSpace(value)
			break
		}
	}
	data, err = os.ReadFile("/proc/meminfo")
	if err != nil {
		return h, err
	}
	var totalKiB int64
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			if _, err := fmt.Sscanf(line, "MemTotal: %d kB", &totalKiB); err != nil {
				return h, err
			}
			break
		}
	}
	if memoryMiB <= 0 || memoryMiB > totalKiB/1024 {
		return h, errors.New("worker memory budget exceeds host memory or is not positive")
	}
	return h, nil
}

func allocate(h Hardware, req Resources, held []Allocation) (Allocation, error) {
	if req.Cores <= 0 || req.MemoryMiB <= 0 {
		return Allocation{}, errors.New("cores and memory_mib must be positive")
	}
	used := map[int]bool{}
	gpuUsed := map[string]bool{}
	if req.GPUs < 0 {
		return Allocation{}, errors.New("GPU count cannot be negative")
	}
	remaining := h.MemoryMiB
	for _, a := range held {
		if a.MemoryMiB < 0 || a.MemoryMiB > remaining {
			return Allocation{}, errors.New("saved reservations exceed the memory budget")
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
