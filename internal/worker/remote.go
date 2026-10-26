package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

type Remote struct {
	Host      string   `json:"host"`
	StateDir  string   `json:"state_dir"`
	CPUs      string   `json:"cpus"`
	MemoryMiB int64    `json:"memory_mib"`
	GPUs      []string `json:"gpus,omitempty"`
	Binary    string   `json:"binary,omitempty"`
}

type Tunnel struct {
	Client   *http.Client
	Info     Info
	cancel   context.CancelFunc
	done     chan error
	dir      string
	once     sync.Once
	closeErr error
	input    *os.File
}

func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func SSH(ctx context.Context, host, script string, stdin io.Reader) ([]byte, error) {
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]*$`).MatchString(host) {
		return nil, errors.New("invalid SSH host alias")
	}
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-oBatchMode=yes", "-oConnectTimeout=10", host, script)
	cmd.WaitDelay = 3 * time.Second
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh %s: %w: %s", host, err, stderr.String())
	}
	return data, nil
}

func Unit(stateDir string) string {
	hash := sha256.Sum256([]byte(stateDir))
	return "ordo-agent-" + hex.EncodeToString(hash[:8])
}

func (r Remote) validate() error {
	if _, err := GPUList(strings.Join(r.GPUs, ",")); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]*$`).MatchString(r.Host) {
		return errors.New("invalid SSH host alias")
	}
	if !regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`).MatchString(r.StateDir) || path.Clean(r.StateDir) != r.StateDir || r.StateDir == "/" {
		return errors.New("remote state must be a clean absolute path without spaces or colons")
	}
	if len(path.Join(r.StateDir, "agent.sock")) > 100 {
		return errors.New("remote state path is too long for a Unix socket")
	}
	if _, err := CPUList(r.CPUs); err != nil {
		return err
	}
	if r.MemoryMiB <= 0 {
		return errors.New("positive remote memory budget required")
	}
	return nil
}

func artifact(file string) (string, elf.Machine, error) {
	build, err := buildinfo.ReadFile(file)
	if err != nil {
		return "", 0, err
	}
	linux := false
	for _, setting := range build.Settings {
		if setting.Key == "GOOS" && setting.Value == "linux" {
			linux = true
		}
	}
	if !linux {
		return "", 0, errors.New("agent artifact must target Linux; supply --binary with a cross-compiled executable")
	}
	f, err := elf.Open(file)
	if err != nil {
		return "", 0, fmt.Errorf("agent binary must be Linux ELF: %w", err)
	}
	defer func() { _ = f.Close() }()
	for _, program := range f.Progs {
		if program.Type == elf.PT_INTERP {
			return "", 0, errors.New("agent binary must be static; build with CGO_ENABLED=0")
		}
	}
	if f.Machine != elf.EM_X86_64 && f.Machine != elf.EM_AARCH64 {
		return "", 0, errors.New("unsupported worker binary architecture")
	}
	input, err := os.Open(file)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = input.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, input); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(hash.Sum(nil)), f.Machine, nil
}

// Bootstrap uploads an immutable executable and reuses, rather than replaces, a live unit.
func Bootstrap(ctx context.Context, r Remote) error {
	if err := r.validate(); err != nil {
		return err
	}
	binary := r.Binary
	var err error
	if binary == "" {
		binary, err = os.Executable()
		if err != nil {
			return err
		}
	}
	digest, machine, err := artifact(binary)
	if err != nil {
		return err
	}
	arch, err := SSH(ctx, r.Host, "uname -m", nil)
	if err != nil {
		return err
	}
	want := "x86_64"
	if machine == elf.EM_AARCH64 {
		want = "aarch64"
	}
	if strings.TrimSpace(string(arch)) != want {
		return fmt.Errorf("agent binary architecture %s does not match worker %s", want, strings.TrimSpace(string(arch)))
	}
	root := Quote(r.StateDir)
	target := path.Join(r.StateDir, "bin", digest)
	setup := "set -eu; umask 077; mkdir -p -- " + root + "; test \"$(stat -c %a " + root + ")\" = 700; mkdir -p -- " + Quote(path.Join(r.StateDir, "bin")) + "; if test -f " + Quote(target) + "; then test \"$(sha256sum " + Quote(target) + " | cut -d ' ' -f 1)\" = " + Quote(digest) + "; printf present; else printf missing; fi"
	present, err := SSH(ctx, r.Host, setup, nil)
	if err != nil {
		return err
	}
	if string(present) == "missing" {
		file, err := os.Open(binary)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		// A unique temporary file avoids corrupting an executable used by an existing agent.
		upload := "set -eu; umask 077; tmp=$(mktemp " + Quote(path.Join(r.StateDir, "bin", "upload.XXXXXX")) + "); trap 'rm -f -- \"$tmp\"' EXIT; cat > \"$tmp\"; test \"$(sha256sum \"$tmp\" | cut -d ' ' -f 1)\" = " + Quote(digest) + "; chmod 500 \"$tmp\"; if test -e " + Quote(target) + "; then test \"$(sha256sum " + Quote(target) + " | cut -d ' ' -f 1)\" = " + Quote(digest) + "; else mv -- \"$tmp\" " + Quote(target) + "; fi"
		if _, err := SSH(ctx, r.Host, upload, file); err != nil {
			return err
		}
	} else if string(present) != "present" {
		return errors.New("unexpected SSH bootstrap output")
	}
	unit := Unit(r.StateDir)
	start := "set -eu; test \"$(loginctl show-user \"$(id -u)\" -p Linger --value)\" = yes; if ! systemctl --user is-active --quiet " + Quote(unit) + "; then systemd-run --user --collect --unit=" + Quote(unit) + " --property=Type=exec --property=Restart=on-failure --property=RestartSec=1 --property=Delegate=yes --property=TimeoutStopSec=20 --setenv=PATH=\"$PATH\" " + Quote(target) + " agent --state " + root + " --cpus " + Quote(r.CPUs) + fmt.Sprintf(" --memory-mib %d", r.MemoryMiB) + " --gpus " + Quote(strings.Join(r.GPUs, ",")) + "; fi"
	if _, err := SSH(ctx, r.Host, start, nil); err != nil {
		return err
	}
	actual, err := SSH(ctx, r.Host, "systemctl --user show "+Quote(unit)+" -p ExecStart --value", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(string(actual), target+" ") && !strings.Contains(string(actual), target+" ;") {
		return fmt.Errorf("worker agent is already using another binary; explicitly stop %s before upgrading (task containers must remain intact)", unit)
	}
	return nil
}

func Connect(ctx context.Context, r Remote) (*Tunnel, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "ordo-tunnel-")
	if err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	t := &Tunnel{cancel: cancel, done: make(chan error, 1), dir: dir}
	socket := filepath.Join(dir, "control.sock")
	t.Client = Client(socket)
	cmd := exec.CommandContext(child, "ssh", "-S", "none", "-T", "-oBatchMode=yes", "-oExitOnForwardFailure=yes", "-oStreamLocalBindUnlink=yes", "-oStreamLocalBindMask=0177", "-oServerAliveInterval=5", "-oServerAliveCountMax=3", "-oConnectTimeout=10", "-L", socket+":"+path.Join(r.StateDir, "agent.sock"), r.Host, "exec cat >/dev/null")
	reader, writer, err := os.Pipe()
	if err != nil {
		cancel()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	t.input = writer
	cmd.Stdin = reader
	defer func() { _ = reader.Close() }()
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		cancel()
		_ = os.RemoveAll(dir)
		return nil, err
	}
	go func() { t.done <- cmd.Wait() }()
	ready, stop := context.WithTimeout(child, 20*time.Second)
	defer stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		requestCtx, end := context.WithTimeout(ready, time.Second)
		err := Call(requestCtx, t.Client, http.MethodGet, "/v1/info", nil, &t.Info)
		end()
		if err == nil {
			if t.Info.Protocol != ProtocolVersion || t.Info.ID == "" {
				_ = t.Close()
				return nil, errors.New("incompatible agent protocol or identity")
			}
			requestedGPUs, _ := GPUList(strings.Join(r.GPUs, ","))
			var actualGPUs []string
			for _, gpu := range t.Info.Hardware.GPUs {
				actualGPUs = append(actualGPUs, gpu.UUID)
			}
			slices.Sort(actualGPUs)
			if !slices.Equal(requestedGPUs, actualGPUs) {
				_ = t.Close()
				return nil, errors.New("live agent GPU pool differs from configuration")
			}
			configured, _ := CPUList(r.CPUs)
			var actual []int
			for _, core := range t.Info.Hardware.Cores {
				actual = append(actual, core...)
			}
			parsed, _ := CPUList(CPUString(actual))
			if CPUString(configured) != CPUString(parsed) || t.Info.Hardware.MemoryMiB != r.MemoryMiB {
				_ = t.Close()
				return nil, errors.New("live agent pool differs from configuration; do not change active reservations implicitly")
			}
			return t, nil
		}
		select {
		case err := <-t.done:
			_ = writer.Close()
			cancel()
			t.Client.CloseIdleConnections()
			_ = os.RemoveAll(dir)
			return nil, fmt.Errorf("SSH tunnel exited: %v: %s", err, stderr.String())
		case <-ready.Done():
			_ = t.Close()
			return nil, fmt.Errorf("agent readiness: %w", ready.Err())
		case <-ticker.C:
		}
	}
}

func (t *Tunnel) Close() error {
	t.once.Do(func() {
		t.Client.CloseIdleConnections()
		// EOF ends the remote transport command even if the controller was killed.
		_ = t.input.Close()
		t.cancel()
		waitErr := <-t.done
		var exit *exec.ExitError
		// Closing the tunnel intentionally terminates SSH and any inherited pipes.
		if errors.As(waitErr, &exit) || errors.Is(waitErr, context.Canceled) || errors.Is(waitErr, exec.ErrWaitDelay) {
			waitErr = nil
		}
		t.closeErr = errors.Join(waitErr, os.RemoveAll(t.dir))
	})
	return t.closeErr
}

type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("agent HTTP %d: %s", e.Status, e.Message) }

func Call(ctx context.Context, client *http.Client, method, endpoint string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://ordo"+endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return &HTTPError{Status: response.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if output == nil {
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	return json.NewDecoder(response.Body).Decode(output)
}
