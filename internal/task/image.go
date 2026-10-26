package task

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/r3clab/Ordo/internal/content"
)

func ValidImageID(id string) bool {
	digest, ok := strings.CutPrefix(id, "sha256:")
	return ok && content.Valid(digest)
}
func ImageID(ctx context.Context, reference string) (string, error) {
	return command(execPodman).imageID(ctx, reference)
}
func (podman command) imageID(ctx context.Context, reference string) (string, error) {
	data, err := podman.output(ctx, "image", "inspect", "--format=json", "--", reference)
	if err != nil {
		return "", err
	}
	var images []struct{ ID string }
	if err := json.Unmarshal(data, &images); err != nil {
		return "", err
	}
	if len(images) != 1 || !content.Valid(strings.TrimPrefix(images[0].ID, "sha256:")) {
		return "", errors.New("expected one immutable local image")
	}
	return "sha256:" + strings.TrimPrefix(images[0].ID, "sha256:"), nil
}
func ExportImage(ctx context.Context, reference, dest string) (string, error) {
	id, err := ImageID(ctx, reference)
	if err != nil {
		return "", err
	}
	_, err = command(execPodman).output(ctx, "save", "--format=oci-archive", "--output", dest, "--", id)
	return id, err
}
func LoadImage(ctx context.Context, archive, want string) error {
	if !ValidImageID(want) {
		return errors.New("invalid image identity")
	}
	if _, err := command(execPodman).output(ctx, "load", "--input", archive); err != nil {
		return err
	}
	id, err := ImageID(ctx, want)
	if err != nil {
		return err
	}
	if id != want {
		return fmt.Errorf("loaded image %s does not match %s", id, want)
	}
	return nil
}
