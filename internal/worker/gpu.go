package worker

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/task"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type GPU struct {
	UUID      string `json:"uuid"`
	Index     int    `json:"index"`
	Model     string `json:"model"`
	MemoryMiB int64  `json:"memory_mib"`
	Driver    string `json:"driver"`
}

func GPUList(text string) ([]string, error) {
	if text == "" {
		return nil, nil
	}
	return task.CheckGPUs(strings.Split(text, ","))
}
func discoverGPUs(ctx context.Context, ids []string, root string) ([]GPU, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if _, err := task.CheckGPUs(ids); err != nil {
		return nil, err
	}
	data, err := exec.CommandContext(ctx, "nvidia-smi", "--query-gpu=index,uuid,name,memory.total,driver_version,mig.mode.current", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("GPU discovery: %w", err)
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		return nil, err
	}
	gpus := []GPU{}
	for _, row := range rows {
		if len(row) != 6 {
			return nil, errors.New("unexpected NVIDIA inventory format")
		}
		for i := range row {
			row[i] = strings.TrimSpace(row[i])
		}
		if !slices.Contains(ids, row[1]) {
			continue
		}
		if row[5] != "Disabled" && row[5] != "[N/A]" {
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
		gpus = append(gpus, GPU{UUID: row[1], Index: index, Model: row[2], MemoryMiB: memory, Driver: row[4]})
	}
	if len(gpus) != len(ids) {
		return nil, errors.New("configured GPU UUID is missing")
	}
	dir := filepath.Join(root, "cdi")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	args := []string{"cdi", "generate", "--format=json", "--device-name-strategy=uuid", "--no-all-device"}
	for _, id := range ids {
		args = append(args, "--device-id", id)
	}
	data, err = exec.CommandContext(ctx, "nvidia-ctk", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("private NVIDIA CDI generation: %w", err)
	}
	var spec struct {
		Kind    string `json:"kind"`
		Devices []struct {
			Name string `json:"name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	if spec.Kind != "nvidia.com/gpu" || len(spec.Devices) != len(ids) {
		return nil, errors.New("CDI specification contains unexpected devices")
	}
	seen := map[string]bool{}
	for _, device := range spec.Devices {
		if !slices.Contains(ids, device.Name) || seen[device.Name] {
			return nil, errors.New("CDI device identity mismatch")
		}
		seen[device.Name] = true
	}
	err = content.WriteAtomic(filepath.Join(dir, "nvidia.json"), func(w io.Writer) error {
		_, err := w.Write(data)
		return err
	})
	if err != nil {
		return nil, err
	}
	return gpus, nil
}
func busyGPUs(ctx context.Context, gpus []GPU) ([]string, error) {
	data, err := exec.CommandContext(ctx, "nvidia-smi", "pmon", "-c", "1").Output()
	if err != nil {
		return nil, fmt.Errorf("check GPU processes: %w", err)
	}
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
