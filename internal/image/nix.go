package image

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Exec func(ctx context.Context, name string, args ...string) ([]byte, error)

// Skopeo must be nix2container's build; stock skopeo lacks the nix: transport.
type Tools struct {
	Nix, Skopeo string
	Exec        Exec
}

func (t Tools) Build(ctx context.Context, installable, link string) (string, error) {
	data, err := t.run(ctx, cmp.Or(t.Nix, "nix"), "--extra-experimental-features", "nix-command flakes", "build", "--out-link", link, "--print-out-paths", "--", installable)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(string(data))
	if out == "" || strings.Contains(out, "\n") || !filepath.IsAbs(out) {
		return "", fmt.Errorf("nix build %s must produce exactly one store path", installable)
	}
	return out, nil
}

func (t Tools) Export(ctx context.Context, out, dest string) error {
	// skopeo reads a colon after oci-archive:PATH as the start of an image reference.
	if strings.Contains(dest, ":") {
		return fmt.Errorf("image export path %s must not contain ':'", dest)
	}
	_, err := t.run(ctx, cmp.Or(t.Skopeo, "skopeo"), "--insecure-policy", "copy", "nix:"+out, "oci-archive:"+dest)
	return err
}

func (t Tools) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if t.Exec != nil {
		return t.Exec(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 10 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return out, nil
}

// Pull needs no signature policy: the reference's digest already pins what it fetches.
func (t Tools) Pull(ctx context.Context, reference, dest string) error {
	if strings.Contains(dest, ":") {
		return fmt.Errorf("image export path %s must not contain ':'", dest)
	}
	_, err := t.run(ctx, cmp.Or(t.Skopeo, "skopeo"), "--insecure-policy", "copy", reference, "oci-archive:"+dest)
	return err
}
