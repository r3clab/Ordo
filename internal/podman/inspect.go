package podman

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/r3clab/Ordo/internal/content"
)

const (
	Running = "running"
	Paused  = "paused"
	Exited  = "exited"
)

// Container is one `podman ps --format json` entry.
type Container struct {
	ID        string            `json:"Id"`
	State     string            `json:"State"`
	Labels    map[string]string `json:"Labels"`
	Created   Time              `json:"Created"`
	StartedAt Time              `json:"StartedAt"`
}

func (c Container) Attempt() string { return c.Labels[AttemptLabel] }

func (c Container) Server() string { return c.Labels[ServerLabel] }

// Time accepts the Unix seconds or RFC 3339 strings that different Podman versions print.
type Time struct{ time.Time }

func (t *Time) UnmarshalJSON(data []byte) error {
	if seconds, err := strconv.ParseInt(string(data), 10, 64); err == nil {
		if seconds > 0 {
			t.Time = time.Unix(seconds, 0).UTC()
		}
		return nil
	}
	return json.Unmarshal(data, &t.Time)
}

func ParsePS(data []byte) ([]Container, error) {
	var containers []Container
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	return containers, nil
}

type Inspection struct {
	ID     string
	Name   string
	Image  string
	Config struct{ Labels map[string]string }
	State  struct {
		Status     string
		ExitCode   *int
		OOMKilled  bool
		StartedAt  time.Time
		FinishedAt time.Time
	}
}

func ParseInspect(data []byte) (Inspection, error) {
	var containers []Inspection
	if err := json.Unmarshal(data, &containers); err != nil {
		return Inspection{}, fmt.Errorf("decode container inspection: %w", err)
	}
	if len(containers) != 1 || !content.Valid(containers[0].ID) {
		return Inspection{}, errors.New("expected exactly one container")
	}
	return containers[0], nil
}

// CheckInfo requires the rootless cgroup v2 controllers that CPU pinning and memory limits depend on.
func CheckInfo(data []byte) error {
	var info struct {
		Host struct {
			OS                string
			ServiceIsRemote   bool
			CgroupVersion     string
			CgroupControllers []string
			Security          struct{ Rootless bool }
		}
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("decode Podman info: %w", err)
	}
	host := info.Host
	if host.OS != "linux" || host.ServiceIsRemote || !host.Security.Rootless {
		return errors.New("execution requires local, rootless Podman on Linux")
	}
	if host.CgroupVersion != "v2" {
		return errors.New("execution requires cgroup v2 for rootless resource limits")
	}
	for _, controller := range []string{"cpuset", "memory"} {
		if !slices.Contains(host.CgroupControllers, controller) {
			return fmt.Errorf("missing delegated %s cgroup controller; configure delegation for the worker's user session", controller)
		}
	}
	return nil
}
