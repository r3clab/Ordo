package image

import (
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
	Skopeo string
	Exec   Exec
}

func (t Tools) Build(ctx context.Context, installable, link string) (string, error) {
	data, err := t.run(ctx, "nix", "--extra-experimental-features", "nix-command flakes", "build", "--out-link", link, "--print-out-paths", "--", installable)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(string(data))
	if out == "" || strings.Contains(out, "\n") || !filepath.IsAbs(out) {
		return "", fmt.Errorf("nix build %s must produce exactly one store path", installable)
	}
	return out, nil
}

// Copy needs no signature policy: src is a local nix: output or a digest-pinned reference.
func (t Tools) Copy(ctx context.Context, src, dest string) error {
	// skopeo reads a colon after oci-archive:PATH as the start of an image reference.
	if strings.Contains(dest, ":") {
		return fmt.Errorf("image export path %s must not contain ':'", dest)
	}
	_, err := t.run(ctx, cmp.Or(t.Skopeo, "skopeo"), "--insecure-policy", "copy", src, "oci-archive:"+dest)
	return err
}

func (t Tools) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	if t.Exec != nil {
		return t.Exec(ctx, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 10 * time.Second
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("%s: %w", filepath.Base(name), err)
	}
	return out, nil
}
