// Package ssh runs shell scripts on a worker through one multiplexed OpenSSH connection.
package ssh

import (
	"bytes"
	"cmp"
	"context"
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

// MaxCommand stays below Linux's 128 KiB limit on one argv string, which the remote shell's -c receives.
const MaxCommand = 120 << 10

// ErrTransport marks failures that say nothing about the remote command, so callers reconnect.
var ErrTransport = errors.New("SSH transport failed")

var host = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]*$`)

func ValidHost(alias string) bool { return host.MatchString(alias) }

func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func command(script string) string { return "exec sh -c " + Quote(script) }

// config is an ssh_config file that replaces the user's, so workers stay out of everyday SSH aliases.
func client(ctx context.Context, config string, args ...string) *exec.Cmd {
	if config != "" {
		args = append([]string{"-F", config}, args...)
	}
	return exec.CommandContext(ctx, "ssh", args...)
}

func CheckScript(script string) error {
	if size := len(command(script)); size > MaxCommand {
		return fmt.Errorf("remote command is %d bytes; the limit is %d", size, MaxCommand)
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
	return fmt.Sprintf("remote command exited with status %d: %s", e.Code, e.Stderr)
}

type Master struct {
	host   string
	config string
	socket string
	input  *os.File
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	stderr bytes.Buffer
	once   sync.Once
}

// Dial starts a master connection whose remote command ends when this process closes its stdin, so a
// killed server leaves no SSH session or forward behind.
func Dial(ctx context.Context, alias, config string) (*Master, error) {
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
	m := &Master{host: alias, config: config, socket: filepath.Join(dir, "master"), input: writer, cancel: cancel, done: make(chan struct{})}
	// Forwarded sockets are mode 0600, so only the worker user's containers can connect to a service.
	cmd := client(child, config, "-M", "-S", m.socket, "-T", "-oBatchMode=yes", "-oControlPersist=no", "-oExitOnForwardFailure=yes", "-oStreamLocalBindMask=0177", "-oServerAliveInterval=5", "-oServerAliveCountMax=3", "-oConnectTimeout=10", alias, "exec cat >/dev/null")
	cmd.Stdin = reader
	cmd.Stderr = &m.stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.Join(err, writer.Close(), os.RemoveAll(dir))
	}
	go func() {
		m.err = cmd.Wait()
		close(m.done)
	}()
	ready, stop := context.WithTimeout(child, 20*time.Second)
	defer stop()
	for {
		check := client(ready, config, "-S", m.socket, "-O", "check", alias)
		if check.Run() == nil {
			return m, nil
		}
		select {
		case <-m.done:
			_ = m.Close()
			return nil, transport(alias, "%v: %s", m.err, strings.TrimSpace(m.stderr.String()))
		case <-ready.Done():
			_ = m.Close()
			return nil, transport(alias, "master not ready: %v", ready.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// The last 4 KiB of stderr also reach the returned error, even when stderr is nil.
func (m *Master) Run(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := CheckScript(script); err != nil {
		return err
	}
	// ssh silently connects directly when the master is gone, which would bypass its forwards.
	select {
	case <-m.done:
		return transport(m.host, "master connection closed")
	default:
	}
	tail := &tailBuffer{}
	cmd := client(ctx, m.config, "-S", m.socket, "-oControlMaster=no", "-T", "-oBatchMode=yes", "-oLogLevel=ERROR", m.host, command(script))
	cmd.Stdin = stdin
	var local []*sink
	catch := func(w io.Writer) io.Writer {
		s := &sink{w: w}
		local = append(local, s)
		return s
	}
	if stdout != nil {
		cmd.Stdout = catch(stdout)
	}
	cmd.Stderr = tail
	if stderr != nil {
		cmd.Stderr = io.MultiWriter(catch(stderr), tail)
	}
	cmd.WaitDelay = 3 * time.Second
	err := cmd.Run()
	if err == nil || ctx.Err() != nil {
		return cmp.Or(ctx.Err(), err)
	}
	// A failed local write kills ssh like a broken connection would, but the fault is the caller's.
	for _, s := range local {
		if s.err != nil {
			return s.err
		}
	}
	// ssh exits 255 for its own failures, and a signal (status -1) ends ssh, not the remote command;
	// any other status is the remote command's.
	if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() > 0 && exit.ExitCode() != 255 {
		return &ExitError{Code: exit.ExitCode(), Stderr: strings.TrimSpace(tail.String())}
	}
	return transport(m.host, "%v: %s", err, strings.TrimSpace(tail.String()))
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

// remote is ssh's -R argument, such as SOCKET:HOST:PORT; the forward lives as long as the master.
func (m *Master) Forward(ctx context.Context, remote string) error {
	out, err := client(ctx, m.config, "-S", m.socket, "-O", "forward", "-R", remote, m.host).CombinedOutput()
	if err != nil {
		return transport(m.host, "forward %s: %v: %s", remote, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Master) Close() error {
	m.once.Do(func() {
		_ = m.input.Close()
		m.cancel()
		<-m.done
		_ = os.RemoveAll(filepath.Dir(m.socket))
	})
	return nil
}

// tailBuffer keeps the end of stderr, where tools print the error that ended them.
type tailBuffer struct{ data []byte }

const tailBytes = 4 << 10

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.data = append(b.data, p...)
	if len(b.data) > tailBytes {
		b.data = b.data[len(b.data)-tailBytes:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.data) }

type sink struct {
	w   io.Writer
	err error
}

func (s *sink) Write(p []byte) (int, error) {
	n, err := s.w.Write(p)
	if err != nil && s.err == nil {
		s.err = err
	}
	return n, err
}
