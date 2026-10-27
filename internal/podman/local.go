package podman

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/r3clab/Ordo/internal/content"
)

func ImageID(ctx context.Context, reference string) (string, error) {
	data, err := local(ctx, "image", "inspect", "--format=json", "--", reference)
	if err != nil {
		return "", err
	}
	return ParseImageID(data)
}

func ParseImageID(data []byte) (string, error) {
	var images []struct{ ID string }
	if err := json.Unmarshal(data, &images); err != nil {
		return "", err
	}
	if len(images) != 1 || !content.Valid(strings.TrimPrefix(images[0].ID, "sha256:")) {
		return "", errors.New("expected one immutable local image")
	}
	return "sha256:" + strings.TrimPrefix(images[0].ID, "sha256:"), nil
}

func SaveImage(ctx context.Context, id, dest string) error {
	_, err := local(ctx, "save", "--format=oci-archive", "--output", dest, "--", id)
	return err
}

func local(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "podman", append([]string{"--remote=false"}, args...)...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("podman %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
