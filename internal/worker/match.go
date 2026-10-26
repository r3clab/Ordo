package worker

import (
	"errors"
	"slices"
)

var architectures = []string{"amd64", "arm64"}

func Architectures() []string { return slices.Clone(architectures) }

type Match struct {
	Arch            string   `json:"arch,omitempty"`
	CPUModel        string   `json:"cpu_model,omitempty"`
	GPUModel        string   `json:"gpu_model,omitempty"`
	MinGPUMemoryMiB int64    `json:"min_gpu_memory_mib,omitempty"`
	GPUDriver       string   `json:"gpu_driver,omitempty"`
	Tags            []string `json:"tags,omitempty"`
}

func (m Match) Validate(resources Resources) error {
	if m.Arch != "" && !slices.Contains(architectures, m.Arch) {
		return errors.New("match.arch must be amd64 or arm64")
	}
	if m.MinGPUMemoryMiB < 0 {
		return errors.New("GPU memory requirement cannot be negative")
	}
	if resources.GPUs == 0 && (m.GPUModel != "" || m.MinGPUMemoryMiB > 0 || m.GPUDriver != "") {
		return errors.New("GPU matching requires a positive GPU request")
	}
	if !ValidNameSet(m.Tags) {
		return errors.New("match.tags must be distinct names")
	}
	return nil
}

func matchedHardware(h Hardware, m *Match) (Hardware, bool) {
	if m == nil {
		return h, true
	}
	if (m.Arch != "" && h.Arch != m.Arch) || (m.CPUModel != "" && h.CPUModel != m.CPUModel) {
		return h, false
	}
	all := h.GPUs
	h.GPUs = nil
	for _, gpu := range all {
		if (m.GPUModel == "" || gpu.Model == m.GPUModel) && gpu.MemoryMiB >= m.MinGPUMemoryMiB && (m.GPUDriver == "" || gpu.Driver == m.GPUDriver) {
			h.GPUs = append(h.GPUs, gpu)
		}
	}
	return h, true
}
func FitsSpec(h Hardware, spec Spec, held []Allocation) bool {
	filtered, ok := matchedHardware(h, spec.Match)
	if !ok {
		return false
	}
	_, err := allocate(filtered, spec.Resources, held)
	return err == nil
}
