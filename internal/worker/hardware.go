package worker

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

type Hardware struct {
	Hostname  string  `json:"hostname"`
	MachineID string  `json:"machine_id"`
	Arch      string  `json:"arch"`
	CPUModel  string  `json:"cpu_model"`
	Cores     [][]int `json:"cores"`
	MemoryMiB int64   `json:"memory_mib"`
}

type Resources struct {
	Cores     int   `json:"cores"`
	MemoryMiB int64 `json:"memory_mib"`
}

type Allocation struct {
	CPUs      []int `json:"cpus"`
	MemoryMiB int64 `json:"memory_mib"`
}

func CPUList(text string) ([]int, error) {
	set := map[int]bool{}
	for _, part := range strings.Split(text, ",") {
		a, b, span := strings.Cut(part, "-")
		lo, err := strconv.Atoi(a)
		if err != nil || lo < 0 || lo > 65535 {
			return nil, fmt.Errorf("invalid CPU set %q", text)
		}
		hi := lo
		if span {
			hi, err = strconv.Atoi(b)
			if err != nil || hi < lo || hi > 65535 {
				return nil, fmt.Errorf("invalid CPU range %q", part)
			}
		}
		for n := lo; n <= hi; n++ {
			if set[n] {
				return nil, fmt.Errorf("duplicate CPU %d", n)
			}
			set[n] = true
		}
	}
	result := make([]int, 0, len(set))
	for n := range set {
		result = append(result, n)
	}
	slices.Sort(result)
	return result, nil
}

func CPUString(cpus []int) string {
	parts := make([]string, len(cpus))
	for i, cpu := range cpus {
		parts[i] = strconv.Itoa(cpu)
	}
	return strings.Join(parts, ",")
}

func discover(cpus string, memoryMiB int64) (Hardware, error) {
	if runtime.GOOS != "linux" {
		return Hardware{}, errors.New("worker execution requires Linux")
	}
	pool, err := CPUList(cpus)
	if err != nil {
		return Hardware{}, err
	}
	onlineData, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return Hardware{}, err
	}
	online, err := CPUList(strings.TrimSpace(string(onlineData)))
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
		siblings, err := CPUList(strings.TrimSpace(string(data)))
		if err != nil {
			return h, err
		}
		for _, n := range siblings {
			if !slices.Contains(pool, n) || !slices.Contains(online, n) {
				return h, fmt.Errorf("CPU %d requires all online SMT siblings %s in the pool", cpu, CPUString(siblings))
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
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "MemTotal:") {
			if _, err := fmt.Sscanf(scanner.Text(), "MemTotal: %d kB", &totalKiB); err != nil {
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

func Fits(h Hardware, req Resources, held []Allocation) bool {
	_, err := allocate(h, req, held)
	return err == nil
}

func allocate(h Hardware, req Resources, held []Allocation) (Allocation, error) {
	if req.Cores <= 0 || req.MemoryMiB <= 0 {
		return Allocation{}, errors.New("cores and memory_mib must be positive")
	}
	used := map[int]bool{}
	remaining := h.MemoryMiB
	for _, a := range held {
		if a.MemoryMiB < 0 || a.MemoryMiB > remaining {
			return Allocation{}, errors.New("saved reservations exceed the memory budget")
		}
		remaining -= a.MemoryMiB
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
			return a, nil
		}
	}
	return Allocation{}, ErrCapacity
}
