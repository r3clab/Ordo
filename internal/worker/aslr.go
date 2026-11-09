package worker

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"syscall"

	"github.com/r3clab/Ordo/internal/content"
)

// Every exec that changes AppArmor or SELinux profile clears ADDR_NO_RANDOMIZE, and Podman and crun
// each run under their own profile on Ubuntu, so only a process inside the container keeps it. Ordo's
// own binary is that process: Podman mounts it as the container's init, which disables ASLR and execs
// the task. crun applies only a personality's domain, never its flags.
const noRandomize = 0x0040000

// InitName is the basename Podman runs --init-path as, at /run/podman-init.
const InitName = "podman-init"

// defaultSeccomp is Podman's default profile from go.podman.io/common v0.70.0, for Podman builds that
// compile theirs in and report no profile file.
//
//go:embed seccomp.json
var defaultSeccomp []byte

// Init is the container's init when a step disables ASLR; Podman passes it "--" and the task's argv.
func Init(args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return errors.New("no command")
	}
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PERSONALITY, noRandomize, 0, 0); errno != 0 {
		return fmt.Errorf("disable ASLR: %w", errno)
	}
	file, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(file, args, os.Environ())
}

type aslrFiles struct{ init, seccomp string }

var self = sync.OnceValues(func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	f, err := os.Open(exe)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
})

// aslrFiles installs the init and seccomp profile a launch that disables ASLR mounts. The init is
// world-executable, as a task may run as an image user that the worker's UID maps elsewhere.
func (h *Host) aslrFiles(ctx context.Context) (aslrFiles, error) {
	h.aslr.Lock()
	defer h.aslr.Unlock()
	digest, err := self()
	if err != nil {
		return aslrFiles{}, err
	}
	init := h.initPath(digest)
	if info, err := os.Stat(init); err != nil || info.Mode().Perm() != 0o755 {
		exe, err := os.Executable()
		if err != nil {
			return aslrFiles{}, err
		}
		if err := install(init, 0o755, func(w io.Writer) error { return copyFrom(w, exe) }); err != nil {
			return aslrFiles{}, err
		}
	}
	profile, err := h.seccompProfile(ctx)
	if err != nil {
		return aslrFiles{}, err
	}
	sum := sha256.Sum256(profile)
	seccomp := h.seccompPath(hex.EncodeToString(sum[:]))
	if _, err := os.Stat(seccomp); err != nil {
		if err := install(seccomp, 0o600, func(w io.Writer) error { _, err := w.Write(profile); return err }); err != nil {
			return aslrFiles{}, err
		}
	}
	return aslrFiles{init: init, seccomp: seccomp}, nil
}

// seccompProfile extends the profile Podman would apply with personality(ADDR_NO_RANDOMIZE) alone.
func (h *Host) seccompProfile(ctx context.Context) ([]byte, error) {
	out, err := h.output(ctx, "podman --remote=false info --format '{{.Host.Security.SECCOMPProfilePath}}'", nil)
	if err != nil {
		return nil, err
	}
	base := defaultSeccomp
	if file := strings.TrimSpace(string(out)); file != "" {
		if base, err = os.ReadFile(file); err != nil {
			return nil, err
		}
	}
	var profile map[string]any
	if err := json.Unmarshal(base, &profile); err != nil {
		return nil, fmt.Errorf("seccomp profile: %w", err)
	}
	syscalls, _ := profile["syscalls"].([]any)
	profile["syscalls"] = append(syscalls, map[string]any{"names": []string{"personality"}, "action": "SCMP_ACT_ALLOW", "args": []map[string]any{{"index": 0, "value": noRandomize, "op": "SCMP_CMP_EQ"}}})
	return json.Marshal(profile)
}

func install(dest string, mode os.FileMode, write func(io.Writer) error) error {
	if err := os.MkdirAll(path.Dir(dest), 0o700); err != nil {
		return err
	}
	if err := content.WriteAtomic(dest, write); err != nil {
		return err
	}
	return os.Chmod(dest, mode)
}

func copyFrom(w io.Writer, file string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(w, f)
	return err
}
