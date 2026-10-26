package task

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Record is the existing, inspectable local result format.
type Record = record

func CheckHost(ctx context.Context) error {
	data, err := command(execPodman).output(ctx, "info", "--format=json")
	if err != nil {
		return err
	}
	return checkHost(data)
}

func ReadResult(dir string) (Record, error) {
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return Record{}, err
	}
	var r Record
	err = json.Unmarshal(data, &r)
	return r, err
}

// Stop targets only a verified existing execution. Absence is not proof of cancellation.
func Stop(ctx context.Context, dir string) error {
	r, status, err := Status(ctx, dir)
	if err != nil {
		return err
	}
	switch status {
	case "removed", "exited":
		return nil
	case "running", "paused", "created", "configured", "stopping":
		c, err := command(execPodman).inspectOwned(ctx, &r, cmp.Or(r.ContainerID, r.ContainerName))
		if err != nil {
			return err
		}
		_, err = command(execPodman).output(ctx, "stop", "--time=10", "--", c.ID)
		return err
	default:
		return errors.New("execution is not yet identifiable; cancellation remains pending")
	}
}

// Status inspects an existing execution; it never creates or starts a container.
func Status(ctx context.Context, dir string) (Record, string, error) {
	r, err := ReadResult(dir)
	if err != nil {
		return r, "", err
	}
	if r.ImageID == "" && r.ContainerID == "" && strings.HasPrefix(r.ContainerName, "ordo-") && (r.State == "preparing" || r.State == "error" || r.State == "interrupted") {
		return r, "unstarted", nil
	}
	checked, err := loadRecord(dir)
	if err != nil {
		return r, "", err
	}
	r = *checked
	if r.ContainerRemoved {
		return r, "removed", nil
	}
	selector := cmp.Or(r.ContainerID, r.ContainerName)
	p := command(execPodman)
	exists, err := p.containerExists(ctx, selector)
	if err != nil {
		return r, "", err
	}
	if !exists {
		return r, "missing", nil
	}
	c, err := p.inspectOwned(ctx, &r, selector)
	if err != nil {
		return r, "", err
	}
	return r, c.State.Status, nil
}
