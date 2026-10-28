package worker

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/ssh"
)

// gpus installs a private CDI specification for exactly the configured devices, so Podman can hand a
// task only the GPUs it was allocated without a global CDI registration.
func (h *Host) gpus(ctx context.Context) ([]pool.GPU, error) {
	data, err := h.output(ctx, "nvidia-smi --query-gpu=index,uuid,name,memory.total,mig.mode.current --format=csv,noheader,nounits", nil)
	if err != nil {
		return nil, err
	}
	gpus, err := parseGPUs(data, h.GPUs)
	if err != nil {
		return nil, err
	}
	generate := "nvidia-ctk cdi generate --format=json --device-name-strategy=uuid --no-all-device"
	for _, id := range h.GPUs {
		generate += " --device-id " + ssh.Quote(id)
	}
	cdi, err := h.output(ctx, generate, nil)
	if err != nil {
		return nil, err
	}
	if err := checkCDI(cdi, h.GPUs); err != nil {
		return nil, err
	}
	install := "set -eu; umask 077; d=" + ssh.Quote(h.cdiDir()) + "; cat > \"$d/.nvidia.json.tmp\"; mv -f -- \"$d/.nvidia.json.tmp\" \"$d/nvidia.json\""
	return gpus, h.run.Run(ctx, install, bytes.NewReader(cdi), nil, nil)
}

func parseGPUs(data []byte, ids []string) ([]pool.GPU, error) {
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		return nil, err
	}
	gpus := []pool.GPU{}
	for _, row := range rows {
		if len(row) != 5 {
			return nil, errors.New("unexpected NVIDIA inventory format")
		}
		for i := range row {
			row[i] = strings.TrimSpace(row[i])
		}
		if !slices.Contains(ids, row[1]) {
			continue
		}
		if row[4] != "Disabled" && row[4] != "[N/A]" {
			return nil, errors.New("MIG allocations are unsupported")
		}
		index, err := strconv.Atoi(row[0])
		if err != nil {
			return nil, err
		}
		memory, err := strconv.ParseInt(row[3], 10, 64)
		if err != nil {
			return nil, err
		}
		gpus = append(gpus, pool.GPU{UUID: row[1], Index: index, Model: row[2], MemoryMiB: memory})
	}
	if len(gpus) != len(ids) {
		return nil, errors.New("configured GPU UUID is missing")
	}
	return gpus, nil
}

func checkCDI(data []byte, ids []string) error {
	var spec struct {
		Kind    string `json:"kind"`
		Devices []struct {
			Name string `json:"name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return err
	}
	if spec.Kind != "nvidia.com/gpu" || len(spec.Devices) != len(ids) {
		return errors.New("CDI specification contains unexpected devices")
	}
	var names []string
	for _, device := range spec.Devices {
		names = append(names, device.Name)
	}
	if !slices.Equal(slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(ids))) {
		return errors.New("CDI device identity mismatch")
	}
	return nil
}

// GPUBusy lists configured GPUs that run any compute or graphics process, Ordo's or not.
func (h *Host) GPUBusy(ctx context.Context, gpus []pool.GPU) ([]string, error) {
	data, err := h.output(ctx, "nvidia-smi pmon -c 1", nil)
	if err != nil {
		return nil, err
	}
	return parseBusy(data, gpus)
}

func parseBusy(data []byte, gpus []pool.GPU) ([]string, error) {
	seen := map[int]bool{}
	busy := map[int]bool{}
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) < 3 {
			return nil, errors.New("unrecognized GPU process inventory")
		}
		index, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, err
		}
		seen[index] = true
		if fields[1] != "-" {
			if _, err := strconv.Atoi(fields[1]); err != nil {
				return nil, errors.New("GPU process state is unavailable")
			}
			busy[index] = true
		}
	}
	var result []string
	for _, gpu := range gpus {
		if !seen[gpu.Index] {
			return nil, errors.New("GPU process inventory incomplete")
		}
		if busy[gpu.Index] {
			result = append(result, gpu.UUID)
		}
	}
	return result, nil
}
