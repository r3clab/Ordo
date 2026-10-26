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
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/task"
)

var hostAlias = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]*$`)
var remoteStateDir = regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`)
var serviceHost = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.-]*$`)

// Unix socket paths stop at 108 bytes, and ssh's temporary bind names are longer than the final path.
const maxSocketPath = 100

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

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func runSSH(ctx context.Context, host, script string, stdin io.Reader) ([]byte, error) {
	if !hostAlias.MatchString(host) {
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

func unitName(stateDir string) string {
	hash := sha256.Sum256([]byte(stateDir))
	return "ordo-agent-" + hex.EncodeToString(hash[:8])
}

func (r Remote) validate() error {
	if _, err := task.CheckGPUs(r.GPUs); err != nil {
		return err
	}
	if !hostAlias.MatchString(r.Host) {
		return errors.New("invalid SSH host alias")
	}
	if !remoteStateDir.MatchString(r.StateDir) || path.Clean(r.StateDir) != r.StateDir || r.StateDir == "/" {
		return errors.New("remote state must be a clean absolute path without spaces or colons")
	}
	if len(r.agentSocket()) > maxSocketPath {
		return errors.New("remote state path is too long for a Unix socket")
	}
	if _, err := task.ParseCPUs(r.CPUs); err != nil {
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
	if !slices.Contains(build.Settings, debug.BuildSetting{Key: "GOOS", Value: "linux"}) {
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
	digest, err := content.Hash(file)
	return digest, f.Machine, err
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
	arch, err := runSSH(ctx, r.Host, "uname -m", nil)
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
	root := shellQuote(r.StateDir)
	target := path.Join(r.StateDir, "bin", digest)
	setup := "set -eu; umask 077; mkdir -p -- " + root + "; test \"$(stat -c %a " + root + ")\" = 700; mkdir -p -- " + shellQuote(path.Join(r.StateDir, "bin")) + "; if test -f " + shellQuote(target) + "; then test \"$(sha256sum " + shellQuote(target) + " | cut -d ' ' -f 1)\" = " + shellQuote(digest) + "; printf present; else printf missing; fi"
	present, err := runSSH(ctx, r.Host, setup, nil)
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
		upload := "set -eu; umask 077; tmp=$(mktemp " + shellQuote(path.Join(r.StateDir, "bin", "upload.XXXXXX")) + "); trap 'rm -f -- \"$tmp\"' EXIT; cat > \"$tmp\"; test \"$(sha256sum \"$tmp\" | cut -d ' ' -f 1)\" = " + shellQuote(digest) + "; chmod 500 \"$tmp\"; if test -e " + shellQuote(target) + "; then test \"$(sha256sum " + shellQuote(target) + " | cut -d ' ' -f 1)\" = " + shellQuote(digest) + "; else mv -- \"$tmp\" " + shellQuote(target) + "; fi"
		if _, err := runSSH(ctx, r.Host, upload, file); err != nil {
			return err
		}
	} else if string(present) != "present" {
		return errors.New("unexpected SSH bootstrap output")
	}
	unit := unitName(r.StateDir)
	start := "set -eu; test \"$(loginctl show-user \"$(id -u)\" -p Linger --value)\" = yes; if ! systemctl --user is-active --quiet " + shellQuote(unit) + "; then systemd-run --user --collect --unit=" + shellQuote(unit) + " --property=Type=exec --property=Restart=on-failure --property=RestartSec=1 --property=Delegate=yes --property=TimeoutStopSec=20 --setenv=PATH=\"$PATH\" " + shellQuote(target) + " agent --state " + root + " --cpus " + shellQuote(r.CPUs) + fmt.Sprintf(" --memory-mib %d", r.MemoryMiB) + " --gpus " + shellQuote(strings.Join(r.GPUs, ",")) + "; fi"
	if _, err := runSSH(ctx, r.Host, start, nil); err != nil {
		return err
	}
	actual, err := runSSH(ctx, r.Host, "systemctl --user show "+shellQuote(unit)+" -p ExecStart --value", nil)
	if err != nil {
		return err
	}
	if !strings.Contains(string(actual), target+" ") && !strings.Contains(string(actual), target+" ;") {
		return fmt.Errorf("worker agent is already using another binary; explicitly stop %s before upgrading (task containers must remain intact)", unit)
	}
	return nil
}

func (r Remote) agentSocket() string { return path.Join(r.StateDir, "agent.sock") }

func (r Remote) serviceSocket(name string) string {
	return path.Join(r.StateDir, "services", name, "socket")
}

func (r Remote) ServiceForward(name, address string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid service name %q", name)
	}
	socket := r.serviceSocket(name)
	if len(socket) > maxSocketPath {
		return "", fmt.Errorf("service %s socket path is too long for a Unix socket", name)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("service %s: %w", name, err)
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 || (net.ParseIP(host) == nil && !serviceHost.MatchString(host)) {
		return "", fmt.Errorf("service %s needs a HOST:PORT address", name)
	}
	return socket + ":" + net.JoinHostPort(host, port), nil
}

func Connect(ctx context.Context, r Remote, services map[string]string) (*Tunnel, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	var forwards []string
	if len(services) > 0 {
		// sshd keeps a stale socket file by default (StreamLocalBindUnlink no), which would fail the bind.
		prepare := "set -eu; umask 077"
		for _, name := range slices.Sorted(maps.Keys(services)) {
			forward, err := r.ServiceForward(name, services[name])
			if err != nil {
				return nil, err
			}
			socket := r.serviceSocket(name)
			prepare += "; mkdir -p -- " + shellQuote(path.Dir(socket)) + "; rm -f -- " + shellQuote(socket)
			forwards = append(forwards, "-R", forward)
		}
		if _, err := runSSH(ctx, r.Host, prepare, nil); err != nil {
			return nil, err
		}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	dir, err := os.MkdirTemp("", "ordo-tunnel-")
	if err != nil {
		return nil, errors.Join(err, writer.Close())
	}
	child, cancel := context.WithCancel(ctx)
	socket := filepath.Join(dir, "control.sock")
	t := &Tunnel{Client: Client(socket), cancel: cancel, done: make(chan error, 1), dir: dir, input: writer}
	args := []string{"-S", "none", "-T", "-oBatchMode=yes", "-oExitOnForwardFailure=yes", "-oStreamLocalBindUnlink=yes", "-oStreamLocalBindMask=0177", "-oServerAliveInterval=5", "-oServerAliveCountMax=3", "-oConnectTimeout=10", "-L", socket + ":" + r.agentSocket()}
	args = append(append(args, forwards...), r.Host, "exec cat >/dev/null")
	cmd := exec.CommandContext(child, "ssh", args...)
	cmd.Stdin = reader
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.done <- err
		_ = t.Close()
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
			break
		}
		select {
		case err := <-t.done:
			t.done <- err
			_ = t.Close()
			return nil, fmt.Errorf("SSH tunnel exited: %v: %s", err, stderr.String())
		case <-ready.Done():
			_ = t.Close()
			return nil, fmt.Errorf("agent readiness: %w", ready.Err())
		case <-ticker.C:
		}
	}
	if err := t.Info.matches(r); err != nil {
		_ = t.Close()
		return nil, err
	}
	return t, nil
}

func (info Info) matches(r Remote) error {
	if info.Protocol != protocolVersion || info.ID == "" {
		return errors.New("incompatible agent protocol or identity")
	}
	requestedGPUs, _ := task.CheckGPUs(r.GPUs)
	var actualGPUs []string
	for _, gpu := range info.Hardware.GPUs {
		actualGPUs = append(actualGPUs, gpu.UUID)
	}
	slices.Sort(actualGPUs)
	if !slices.Equal(requestedGPUs, actualGPUs) {
		return errors.New("live agent GPU pool differs from configuration")
	}
	configured, _ := task.ParseCPUs(r.CPUs)
	actual := slices.Concat(info.Hardware.Cores...)
	slices.Sort(actual)
	if !slices.Equal(configured, actual) || info.Hardware.MemoryMiB != r.MemoryMiB {
		return errors.New("live agent pool differs from configuration; do not change active reservations implicitly")
	}
	return nil
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
	response, err := Send(ctx, client, method, endpoint, "application/json", body)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			return err
		}
	}
	_, err = io.Copy(io.Discard, response.Body)
	return err
}

func Send(ctx context.Context, client *http.Client, method, endpoint, contentType string, body io.Reader) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, "http://ordo"+endpoint, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if err := checkResponse(response); err != nil {
		return nil, errors.Join(err, response.Body.Close())
	}
	return response, nil
}
