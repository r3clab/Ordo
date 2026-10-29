package ssh

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"
)

// Supervise owns ssh even if the controller is SIGKILLed. EOF on stdin releases the worker lease;
// active forwarded connections get a short grace period, then ssh is killed and reaped.
// This internal subprocess works on both Linux and macOS without parent-death signals.
func Supervise(ctx context.Context, args []string) error {
	reader, writer, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close(); _ = writer.Close() }()
	child, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(child, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = reader, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = reader.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	lease := make(chan struct{})
	go func() { _, _ = io.Copy(writer, os.Stdin); _ = writer.Close(); close(lease) }()
	select {
	case err := <-done:
		return err
	case <-lease:
	case <-ctx.Done():
		_ = writer.Close()
	}
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		cancel()
		return <-done
	}
}
