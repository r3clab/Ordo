// Package ssh bootstraps a connection-scoped worker and forwards its private API socket.
package ssh

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// MaxCommand stays below Linux's 128 KiB limit on one argv string.
const MaxCommand = 120 << 10

// ErrTransport marks failures that say nothing about the remote operation's outcome.
var ErrTransport = errors.New("worker transport failed")

var host = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]*$`)
var socketPath = regexp.MustCompile(`^/[a-zA-Z0-9_./-]+$`)

func ValidHost(alias string) bool  { return host.MatchString(alias) }
func Quote(s string) string        { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
func command(script string) string { return "exec sh -c " + Quote(script) }

func client(ctx context.Context, config string, args ...string) *exec.Cmd {
	if config != "" {
		args = append([]string{"-F", config}, args...)
	}
	return exec.CommandContext(ctx, "ssh", args...)
}

func CheckScript(script string) error {
	if size := len(command(script)); size > MaxCommand {
		return fmt.Errorf("worker command is %d bytes; the limit is %d", size, MaxCommand)
	}
	return nil
}

func transport(host, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrTransport, host, fmt.Sprintf(format, args...))
}

type ExitError struct {
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("worker command exited with status %d: %s", e.Code, e.Stderr)
}

type Master struct {
	host, config, control string
	// Socket is the local, owner-only endpoint of the worker's HTTP API.
	Socket string
	input  *os.File
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	stderr bytes.Buffer
	once   sync.Once
}

// bootstrap uses one exec session for discovery, an atomic binary upload, and the helper itself.
// The digest names the executable, so a reconnect or concurrent upgrade never replaces a running file.
const bootstrap = `set -eu
umask 077
uname -m
IFS=' ' read -r digest size
case "$digest" in *[!0-9a-f]*|'') exit 2;; esac
[ "${#digest}" = 64 ]
case "$size" in *[!0-9]*|'') exit 2;; esac
[ "$size" -gt 0 ] && [ "$size" -le 268435456 ]
d="${XDG_CACHE_HOME:-$HOME/.cache}/ordo/helpers"
mkdir -p -- "$d"
[ "$(stat -c %a "$d")" = 700 ]
bin="$d/$digest"
if [ -f "$bin" ] && [ "$(sha256sum "$bin" | cut -d ' ' -f 1)" = "$digest" ]; then
  printf 'cached\n'
else
  f=$(mktemp "$d/.upload.XXXXXXXX")
  trap 'rm -f -- "$f"' EXIT
  printf 'upload\n'
  head -c "$size" > "$f"
  [ "$(sha256sum "$f" | cut -d ' ' -f 1)" = "$digest" ]
  chmod 700 "$f"
  mv -f -- "$f" "$bin"
  trap - EXIT
fi
exec "$bin" worker
`

// Dial keeps stdin open as a lifetime lease. The helper exits on EOF, including when Ordo is killed.
// executable selects a Linux executable for the architecture reported by the worker.
func Dial(ctx context.Context, alias, config string, init any, executable func(string) (string, error)) (_ *Master, err error) {
	if !ValidHost(alias) {
		return nil, errors.New("invalid SSH host alias")
	}
	dir, err := os.MkdirTemp("", "ordo-ssh-")
	if err != nil {
		return nil, err
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(dir))
	}
	defer func() { _ = reader.Close() }()
	child, cancel := context.WithCancel(ctx)
	m := &Master{host: alias, config: config, control: filepath.Join(dir, "master"), Socket: filepath.Join(dir, "api"), input: writer, cancel: cancel, done: make(chan struct{})}
	cmd := client(child, config, "-M", "-S", m.control, "-T", "-oBatchMode=yes", "-oControlPersist=no", "-oExitOnForwardFailure=yes", "-oStreamLocalBindMask=0177", "-oServerAliveInterval=5", "-oServerAliveCountMax=3", "-oConnectTimeout=10", alias, command(bootstrap))
	self, err := os.Executable()
	if err != nil {
		cancel()
		return nil, errors.Join(err, writer.Close(), os.RemoveAll(dir))
	}
	cmd = exec.CommandContext(child, self, append([]string{"__ssh"}, cmd.Args[1:]...)...)
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.Stdin, cmd.Stderr = reader, &m.stderr
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, errors.Join(err, writer.Close(), os.RemoveAll(dir))
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.Join(err, out.Close(), writer.Close(), os.RemoveAll(dir))
	}
	go func() { m.err = cmd.Wait(); close(m.done) }()
	defer func() {
		if err != nil {
			_ = m.Close()
			err = transport(alias, "%v: %s", err, strings.TrimSpace(m.stderr.String()))
		}
	}()
	// Bound discovery and upload without imposing a deadline on the established connection.
	timer := time.AfterFunc(2*time.Minute, cancel)
	defer timer.Stop()
	scan := bufio.NewScanner(out)
	line := func() (string, error) {
		if scan.Scan() {
			return scan.Text(), nil
		}
		if err := scan.Err(); err != nil {
			return "", err
		}
		return "", errors.New("worker bootstrap ended before readiness")
	}
	arch, err := line()
	if err != nil {
		return nil, err
	}
	binary, err := executable(arch)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(binary)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, f)
	if err != nil {
		return nil, err
	}
	if size == 0 || size > 256<<20 {
		return nil, errors.New("invalid helper executable size")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err = fmt.Fprintf(writer, "%x %d\n", hash.Sum(nil), size); err != nil {
		return nil, err
	}
	status, err := line()
	if err != nil {
		return nil, err
	}
	switch status {
	case "upload":
		if _, err = io.CopyN(writer, f, size); err != nil {
			return nil, err
		}
	case "cached":
	default:
		return nil, fmt.Errorf("unexpected bootstrap response %q", status)
	}
	if err = json.NewEncoder(writer).Encode(init); err != nil {
		return nil, err
	}
	remote, err := line()
	if err != nil {
		return nil, err
	}
	if !socketPath.MatchString(remote) || len(remote) > 100 {
		return nil, errors.New("invalid worker API socket")
	}
	if err = m.forward(child, "-L", m.Socket+":"+remote); err != nil {
		return nil, err
	}
	return m, nil
}

func Home(ctx context.Context, alias, config string) (string, error) {
	if !ValidHost(alias) {
		return "", errors.New("invalid SSH host alias")
	}
	var stderr bytes.Buffer
	cmd := client(ctx, config, "-T", "-oBatchMode=yes", "-oConnectTimeout=10", "-oLogLevel=ERROR", alias, command(`printf %s "$HOME"`))
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", transport(alias, "%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

func (m *Master) forward(ctx context.Context, flag, address string) error {
	out, err := client(ctx, m.config, "-S", m.control, "-O", "forward", flag, address, m.host).CombinedOutput()
	if err != nil {
		return transport(m.host, "forward: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Master) Forward(ctx context.Context, remote string) error {
	return m.forward(ctx, "-R", remote)
}

func (m *Master) Close() error {
	m.once.Do(func() {
		_ = m.input.Close()
		select {
		case <-m.done:
		case <-time.After(3 * time.Second):
		}
		m.cancel()
		<-m.done
		_ = os.RemoveAll(filepath.Dir(m.control))
	})
	return nil
}
