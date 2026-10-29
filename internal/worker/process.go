package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/r3clab/Ordo/internal/ssh"
)

// process executes on the worker; commands never create SSH sessions.
type process struct{}

func (process) Run(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := ssh.CheckScript(script); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Stdin = stdin
	outSink, errSink := &writeFailure{Writer: stdout}, &writeFailure{Writer: stderr}
	if stdout != nil {
		cmd.Stdout = outSink
	}
	tail := &tailBuffer{}
	cmd.Stderr = tail
	if stderr != nil {
		cmd.Stderr = io.MultiWriter(errSink, tail)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 3 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, sink := range []*writeFailure{outSink, errSink} {
		if sink.err != nil {
			return sink.err
		}
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return &ssh.ExitError{Code: exit.ExitCode(), Stderr: strings.TrimSpace(string(tail.data))}
	}
	return err
}

type writeFailure struct {
	io.Writer
	err error
}

func (w *writeFailure) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if w.err == nil {
		w.err = err
	}
	return n, err
}

type tailBuffer struct{ data []byte }

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > 4096 {
		p = p[len(p)-4096:]
	}
	if drop := len(b.data) + len(p) - 4096; drop > 0 {
		b.data = b.data[:copy(b.data, b.data[drop:])]
	}
	b.data = append(b.data, p...)
	return n, nil
}
