package worker

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
)

// backstop is how long after a job's timeout conmon kills the container, which covers a server that
// is down when the timeout passes; while the server is up, its graceful stop comes first.
const backstop = 60

const (
	StdoutFile = "stdout.log"
	StderrFile = "stderr.log"
	OutputsDir = "outputs"
)

const secretsFile = ".secrets.env"

type Runner interface {
	Run(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error
}

type Host struct {
	Config
	run    Runner
	server string

	mu sync.Mutex
	// present caches what EnsureImage and EnsureInput confirmed. Something outside Ordo may remove an
	// image or tree, so a failed launch forgets them all.
	present map[string]bool
}

func New(c Config, run Runner, server string) *Host {
	return &Host{Config: c, run: run, server: server, present: map[string]bool{}}
}

func (h *Host) ensure(key string, put func() error) error {
	h.mu.Lock()
	known := h.present[key]
	h.mu.Unlock()
	if known {
		return nil
	}
	if err := put(); err != nil {
		return err
	}
	h.mu.Lock()
	h.present[key] = true
	h.mu.Unlock()
	return nil
}

func Dial(ctx context.Context, c Config, services map[string]string) (*ssh.Master, error) {
	m, err := ssh.Dial(ctx, c.Host)
	if err != nil {
		return nil, err
	}
	if len(services) == 0 {
		return m, nil
	}
	// sshd keeps a stale socket file by default (StreamLocalBindUnlink no), which would fail the bind.
	prepare := "set -eu; umask 077"
	var forwards []string
	for _, name := range slices.Sorted(maps.Keys(services)) {
		forward, err := c.ServiceForward(name, services[name])
		if err != nil {
			return nil, errors.Join(err, m.Close())
		}
		prepare += "; mkdir -p -- " + ssh.Quote(c.serviceDir(name)) + "; rm -f -- " + ssh.Quote(path.Join(c.serviceDir(name), "socket"))
		forwards = append(forwards, forward)
	}
	if err := m.Run(ctx, prepare, nil, nil, nil); err != nil {
		return nil, errors.Join(err, m.Close())
	}
	for _, forward := range forwards {
		if err := m.Forward(ctx, forward); err != nil {
			return nil, errors.Join(err, m.Close())
		}
	}
	return m, nil
}

// prelude defines pm, which runs Podman in its own transient scope: conmon and fuse-overlayfs stay in
// the caller's cgroup otherwise, and this SSH session's cgroup ends with the session. systemd 254 and
// later expand $VARIABLES in a scope's command line unless told not to, which would rewrite task
// arguments.
func prelude() string {
	return "set -eu; umask 077; X=; if systemd-run --help 2>/dev/null | grep -q -- --expand-environment=; then X=--expand-environment=no; fi; n=0; " +
		"pm() { n=$((n+1)); systemd-run --user --scope --quiet --collect --property=Delegate=yes --unit=ordo-runtime-" + strings.ToLower(rand.Text()) + "-$n $X -- podman --remote=false \"$@\"; }; "
}

func quoteAll(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = ssh.Quote(arg)
	}
	return strings.Join(quoted, " ")
}

func (h *Host) output(ctx context.Context, script string, stdin io.Reader) ([]byte, error) {
	var stdout bytes.Buffer
	err := h.run.Run(ctx, script, stdin, &stdout, nil)
	return stdout.Bytes(), err
}

// absent runs an existence probe, whose exit status 1 means absence.
func (h *Host) absent(ctx context.Context, script string) (bool, error) {
	err := h.run.Run(ctx, script, nil, nil, nil)
	if exit := (*ssh.ExitError)(nil); errors.As(err, &exit) && exit.Code == 1 {
		return true, nil
	}
	return false, err
}

type Snapshot struct {
	BootID string
	// Now is the worker's clock, which container start times also come from.
	Now        time.Time
	Containers []podman.Container
}

func (h *Host) Poll(ctx context.Context) (Snapshot, error) {
	data, err := h.output(ctx, prelude()+"cat /proc/sys/kernel/random/boot_id; date +%s; pm ps -a --filter label="+podman.AttemptLabel+" --format json", nil)
	if err != nil {
		return Snapshot{}, err
	}
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) != 3 {
		return Snapshot{}, errors.New("unexpected worker poll output")
	}
	now, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read worker clock: %w", err)
	}
	containers, err := podman.ParsePS([]byte(lines[2]))
	return Snapshot{BootID: strings.TrimSpace(lines[0]), Now: time.Unix(now, 0).UTC(), Containers: containers}, err
}

func (h *Host) EnsureImage(ctx context.Context, archive, id string) error {
	return h.ensure(id, func() error { return h.loadImage(ctx, archive, id) })
}

func (h *Host) loadImage(ctx context.Context, archive, id string) error {
	if absent, err := h.absent(ctx, prelude()+"pm image exists -- "+ssh.Quote(id)); !absent {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	data, err := h.output(ctx, prelude()+"pm load >&2; pm image inspect --format=json -- "+ssh.Quote(id), f)
	if err != nil {
		return err
	}
	loaded, err := podman.ParseImageID(data)
	if err != nil {
		return err
	}
	if loaded != id {
		return fmt.Errorf("loaded image %s does not match %s", loaded, id)
	}
	return nil
}

func (h *Host) EnsureInput(ctx context.Context, blob, digest string) error {
	return h.ensure(h.treeDir(digest), func() error { return h.unpackInput(ctx, blob, digest) })
}

// unpackInput unpacks a snapshot tar, which the server built from regular files and directories only.
func (h *Host) unpackInput(ctx context.Context, blob, digest string) error {
	tree := ssh.Quote(h.treeDir(digest))
	if absent, err := h.absent(ctx, "test -d "+tree); !absent {
		return err
	}
	f, err := os.Open(blob)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// A task running as a non-root image user gets a subordinate UID, which must still read its inputs.
	script := "set -eu; umask 022; d=$(mktemp -d " + ssh.Quote(h.treeDir(".in.XXXXXX")) + "); trap 'rm -rf -- \"$d\"' EXIT; " +
		"tar -x -f - -C \"$d\"; chmod -R a+rX -- \"$d\"; mv -T -- \"$d\" " + tree + " || test -d " + tree
	return h.run.Run(ctx, script, f, nil, nil)
}

// Seed gives an attempt the outputs of its job's earlier steps before it launches: it moves them out of
// prev's run directory when prev ran here and left them, or unpacks archive otherwise. A repeated seed
// of one attempt finds its outputs in place.
func (h *Host) Seed(ctx context.Context, attempt, prev, archive string) error {
	run := h.runDir(attempt)
	outputs := ssh.Quote(path.Join(run, OutputsDir))
	kept := ssh.Quote(path.Join(h.runDir(prev), OutputsDir))
	script := "set -eu; umask 077; test -d " + outputs + " && exit 0; mkdir -p -- " + ssh.Quote(run) + "; " +
		"if test -d " + kept + "; then mv -T -- " + kept + " " + outputs + "; exit 0; fi; exit 3"
	err := h.run.Run(ctx, script, nil, nil, nil)
	if exit := (*ssh.ExitError)(nil); !errors.As(err, &exit) || exit.Code != 3 {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// -p keeps the modes the earlier step gave its files, which umask would otherwise narrow.
	script = "set -eu; d=$(mktemp -d " + ssh.Quote(path.Join(run, ".seed.XXXXXX")) + "); trap 'rm -rf -- \"$d\"' EXIT; " +
		"tar -x -p -f - -C \"$d\"; mv -T -- \"$d\" " + outputs
	return h.run.Run(ctx, script, f, nil, nil)
}

type Launch struct {
	Attempt    string
	Spec       spec.Spec
	Allocation pool.Allocation
	Secrets    map[string]string
}

// The secrets env file exists only while podman run does.
func launchScript(c Config, l Launch, server string) (string, io.Reader) {
	run := c.runDir(l.Attempt)
	r := podman.Run{Attempt: l.Attempt, Server: server, Image: l.Spec.Image, Args: l.Spec.Args, CPUs: l.Allocation.CPUs, MemoryMiB: l.Allocation.MemoryMiB, Sandbox: l.Spec.Sandbox, Outputs: path.Join(run, OutputsDir), GPUs: l.Allocation.GPUs, CDISpecDir: c.cdiDir()}
	if l.Spec.TimeoutSeconds > 0 {
		r.Timeout = l.Spec.TimeoutSeconds + backstop
	}
	r.Inputs = map[string]string{}
	for name, digest := range l.Spec.Inputs {
		r.Inputs[name] = c.treeDir(digest)
	}
	r.Services = map[string]string{}
	for _, name := range l.Spec.Services {
		r.Services[name] = c.serviceDir(name)
	}
	// A task running as a non-root image user gets a subordinate UID, which must still write its outputs.
	script := prelude() + "mkdir -p -- " + ssh.Quote(r.Outputs) + "; chmod 777 -- " + ssh.Quote(r.Outputs) + "; "
	var stdin io.Reader
	if len(l.Secrets) > 0 {
		r.EnvFile = path.Join(run, secretsFile)
		script += "f=" + ssh.Quote(r.EnvFile) + "; trap 'rm -f -- \"$f\"' EXIT; trap 'exit 129' HUP INT TERM; cat > \"$f\"; "
		var text strings.Builder
		for _, name := range slices.Sorted(maps.Keys(l.Secrets)) {
			text.WriteString(name + "=" + l.Secrets[name] + "\n")
		}
		stdin = strings.NewReader(text.String())
	}
	return script + "pm " + quoteAll(r.Argv()), stdin
}

// A repeated launch of one attempt fails on the container name.
func (h *Host) Launch(ctx context.Context, l Launch) (string, error) {
	script, stdin := launchScript(h.Config, l, h.server)
	data, err := h.output(ctx, script, stdin)
	if err != nil {
		h.mu.Lock()
		clear(h.present)
		h.mu.Unlock()
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !content.Valid(id) {
		return "", fmt.Errorf("container creation returned %q, not an ID", id)
	}
	return id, nil
}

// Stop returns without waiting for the stop; the service name keeps a stop that is still in progress
// from being sent twice.
func (h *Host) Stop(ctx context.Context, attempt string) error {
	unit := ssh.Quote("ordo-stop-" + attempt)
	script := prelude() + "systemctl --user is-active --quiet " + unit + " || systemd-run --user --quiet --collect --unit=" + unit + " $X -- podman --remote=false stop --time=10 -- " + ssh.Quote(podman.Name(attempt))
	return h.run.Run(ctx, script, nil, nil, nil)
}

func (h *Host) Inspect(ctx context.Context, attempt string) (podman.Inspection, error) {
	data, err := h.output(ctx, prelude()+"pm container inspect --format=json -- "+ssh.Quote(podman.Name(attempt)), nil)
	if err != nil {
		return podman.Inspection{}, err
	}
	return podman.ParseInspect(data)
}

// A link or special file in the outputs fails collection, as content.Unpack accepts neither.
func (h *Host) Collect(ctx context.Context, attempt, dir string) error {
	err := writeFiles(filepath.Join(dir, StdoutFile), filepath.Join(dir, StderrFile), func(stdout, stderr io.Writer) error {
		return h.run.Run(ctx, prelude()+"pm logs -- "+ssh.Quote(podman.Name(attempt)), nil, stdout, stderr)
	})
	if err != nil {
		return err
	}
	// sh has no pipefail, so the file list goes through a temporary file to keep find's failures. The
	// POSIX format keeps sub-second modification times.
	script := "set -eu; cd -- " + ssh.Quote(path.Join(h.runDir(attempt), OutputsDir)) + "; l=$(mktemp); trap 'rm -f -- \"$l\"' EXIT; " +
		"find . -mindepth 1 -printf '%P\\0' > \"$l\"; tar -c -f - --format=posix --pax-option=delete=atime,delete=ctime --null --no-recursion --hard-dereference -T \"$l\""
	reader, writer := io.Pipe()
	unpacked := make(chan error, 1)
	go func() {
		err := content.Unpack(reader, filepath.Join(dir, OutputsDir), nil)
		_ = reader.CloseWithError(err)
		unpacked <- err
	}()
	err = h.run.Run(ctx, script, nil, writer, nil)
	_ = writer.CloseWithError(err)
	unpackErr := <-unpacked
	return cmp.Or(err, unpackErr)
}

func writeFiles(first, second string, write func(a, b io.Writer) error) error {
	a, err := os.OpenFile(first, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()
	b, err := os.OpenFile(second, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	if err := write(a, b); err != nil {
		return err
	}
	return errors.Join(a.Sync(), b.Sync())
}

// Callers remove only finished attempts, so a container that still runs is killed.
func (h *Host) Remove(ctx context.Context, attempt string) error {
	return h.run.Run(ctx, prelude()+"pm rm --force --ignore --time=0 -- "+ssh.Quote(podman.Name(attempt))+"; rm -rf -- "+ssh.Quote(h.runDir(attempt)), nil, nil, nil)
}
