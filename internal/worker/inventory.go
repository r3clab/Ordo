package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
)

type Probe struct {
	BootID   string
	Hardware pool.Hardware
}

var architectures = map[string]string{"x86_64": "amd64", "aarch64": "arm64"}

func (h *Host) Probe(ctx context.Context) (Probe, error) {
	script := prelude() + "S=" + ssh.Quote(h.StateDir) + "; mkdir -p -- \"$S\"; if [ \"$(stat -c %a \"$S\")\" != 700 ]; then echo \"$S must be mode 700\" >&2; exit 1; fi; " +
		"mkdir -p -- \"$S/runs\" \"$S/trees\" \"$S/images\" \"$S/cdi\"; t() { printf '\\n@@%s\\n' \"$1\"; }; " +
		"t machine; cat /etc/machine-id; t boot; cat /proc/sys/kernel/random/boot_id; t hostname; uname -n; t arch; uname -m; " +
		"t online; cat /sys/devices/system/cpu/online; " +
		"t cpu; grep -m1 -E '^(model name|CPU implementer)' /proc/cpuinfo || true; t memory; grep -m1 '^MemTotal:' /proc/meminfo; " +
		"t linger; loginctl show-user \"$(id -u)\" -p Linger --value; t podman; pm info --format=json"
	data, err := h.output(ctx, script, nil)
	if err != nil {
		return Probe{}, err
	}
	p, err := parseProbe(sections(string(data)), h.cpus(), h.MemoryMiB)
	if err != nil || len(h.GPUs) == 0 {
		return p, err
	}
	p.Hardware.GPUs, err = h.gpus(ctx)
	return p, err
}

func sections(text string) map[string]string {
	out := map[string]string{}
	name := ""
	for line := range strings.SplitSeq(text, "\n") {
		if header, ok := strings.CutPrefix(line, "@@"); ok {
			name = header
			continue
		}
		out[name] += line + "\n"
	}
	for key, value := range out {
		out[key] = strings.TrimSpace(value)
	}
	return out
}

func parseProbe(s map[string]string, cpus []int, memoryMiB int64) (Probe, error) {
	h := pool.Hardware{Hostname: s["hostname"], MachineID: s["machine"], CPUModel: s["cpu"], MemoryMiB: memoryMiB}
	p := Probe{BootID: s["boot"], Hardware: h}
	if h.MachineID == "" || p.BootID == "" {
		return p, errors.New("worker machine or boot ID is empty")
	}
	if _, model, ok := strings.Cut(h.CPUModel, ":"); ok {
		p.Hardware.CPUModel = strings.TrimSpace(model)
	}
	p.Hardware.Arch = s["arch"]
	if arch, ok := architectures[s["arch"]]; ok {
		p.Hardware.Arch = arch
	}
	online, err := spec.ParseCPUs(s["online"])
	if err != nil {
		return p, err
	}
	for _, cpu := range cpus {
		if !slices.Contains(online, cpu) {
			return p, fmt.Errorf("CPU %d is not online", cpu)
		}
	}
	p.Hardware.CPUs = cpus
	var totalKiB int64
	if _, err := fmt.Sscanf(s["memory"], "MemTotal: %d kB", &totalKiB); err != nil {
		return p, fmt.Errorf("read worker memory: %w", err)
	}
	if memoryMiB > totalKiB/1024 {
		return p, errors.New("worker memory budget exceeds host memory")
	}
	if s["linger"] != "yes" {
		return p, errors.New("tasks would end with the SSH session; enable lingering for the worker user (loginctl enable-linger)")
	}
	return p, podman.CheckInfo([]byte(s["podman"]))
}
