package spec

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const maxCPU = 65535

var gpuUUID = regexp.MustCompile(`^GPU-[a-fA-F0-9-]{36}$`)

func ParseCPUs(text string) ([]int, error) {
	set := map[int]bool{}
	for part := range strings.SplitSeq(text, ",") {
		a, b, span := strings.Cut(part, "-")
		lo, err := strconv.Atoi(a)
		if err != nil || lo < 0 || lo > maxCPU {
			return nil, fmt.Errorf("invalid CPU set %q; use CPU IDs or ranges, such as 0,2-3", text)
		}
		hi := lo
		if span {
			hi, err = strconv.Atoi(b)
			if err != nil || hi < lo || hi > maxCPU {
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
	return slices.Sorted(maps.Keys(set)), nil
}

func FormatCPUs(cpus []int) string {
	parts := make([]string, len(cpus))
	for i, cpu := range cpus {
		parts[i] = strconv.Itoa(cpu)
	}
	return strings.Join(parts, ",")
}

func ValidGPUSet(ids []string) bool {
	for i, id := range ids {
		if !gpuUUID.MatchString(id) || slices.Contains(ids[:i], id) {
			return false
		}
	}
	return true
}
