package worker

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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

type Runner interface {
	Run(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error
}

type Host struct {
	Config
	run    Runner
	server string

	mu sync.Mutex
	// External deletion is possible, so a failed launch invalidates these presence probes.
	present map[string]bool
}

func New(c Config, run Runner, server string) *Host {
	return &Host{Config: c, run: run, server: server, present: map[string]bool{}}
}

func (h *Host) cached(key string, probe func() (bool, error)) (bool, error) {
	h.mu.Lock()
	known := h.present[key]
	h.mu.Unlock()
	if known {
		return true, nil
	}
	present, err := probe()
	if present && err == nil {
		h.mu.Lock()
		h.present[key] = true
		h.mu.Unlock()
	}
	return present, err
}

func (h *Host) forget() {
	h.mu.Lock()
	clear(h.present)
	h.mu.Unlock()
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

// absent runs an existence probe, whose exit status code means absence.
func (h *Host) absent(ctx context.Context, script string, code int) (bool, error) {
	err := h.run.Run(ctx, script, nil, nil, nil)
	if exit, ok := errors.AsType[*ssh.ExitError](err); ok && exit.Code == code {
		return true, nil
	}
	return false, err
}

type Snapshot struct {
	BootID string
	// Now is the worker's clock, which container start times also come from.
	Now        time.Time
	Containers []podman.Container
	// Runs names the attempts that have a run directory.
	Runs []string
}

func (h *Host) Poll(ctx context.Context) (Snapshot, error) {
	runs := ssh.Quote(path.Join(h.StateDir, "runs"))
	data, err := h.output(ctx, prelude()+"cat /proc/sys/kernel/random/boot_id; date +%s; ls -A -- "+runs+" 2>/dev/null | tr '\\n' ' '; echo; pm ps -a --filter label="+podman.AttemptLabel+" --format json", nil)
	if err != nil {
		return Snapshot{}, err
	}
	lines := strings.SplitN(string(data), "\n", 4)
	if len(lines) != 4 {
		return Snapshot{}, errors.New("unexpected worker poll output")
	}
	now, err := strconv.ParseInt(lines[1], 10, 64)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read worker clock: %w", err)
	}
	containers, err := podman.ParsePS([]byte(lines[3]))
	return Snapshot{BootID: strings.TrimSpace(lines[0]), Now: time.Unix(now, 0).UTC(), Containers: containers, Runs: strings.Fields(lines[2])}, err
}

func (h *Host) importImage(ctx context.Context, archive io.Reader, id string) error {
	data, err := h.output(ctx, prelude()+"pm load >&2; mkdir -p -- "+ssh.Quote(h.imageDir())+"; : > "+ssh.Quote(h.imageMark(id))+"; pm image inspect --format=json -- "+ssh.Quote(id), archive)
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

// seed reuses outputs left on this worker; false asks the controller to upload its saved archive.
func (h *Host) seed(ctx context.Context, attempt, prev string) (bool, error) {
	run := h.runDir(attempt)
	outputs := ssh.Quote(path.Join(run, OutputsDir))
	kept := ssh.Quote(path.Join(h.runDir(prev), OutputsDir))
	script := "set -eu; umask 077; test -d " + outputs + " && exit 0; mkdir -p -- " + ssh.Quote(run) + "; " +
		"if test -d " + kept + "; then mv -T -- " + kept + " " + outputs + "; rm -rf -- " + ssh.Quote(h.runDir(prev)) + "; exit 0; fi; exit 3"
	absent, err := h.absent(ctx, script, 3)
	return !absent, err
}

// receiveTree never publishes a partial upload or follows an archive's links outside its staging tree.
func receiveTree(ctx context.Context, dest string, in io.Reader, digest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(dest), ".upload-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	hash := sha256.New()
	in = io.TeeReader(in, hash)
	if err := content.Extract(in, dir); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, in); err != nil {
		return err
	}
	if digest != "" && fmt.Sprintf("%x", hash.Sum(nil)) != digest {
		return errors.New("input archive digest mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Mapped container users must be able to traverse the input root.
	if err := os.Chmod(dir, 0o755); err != nil {
		return err
	}
	if err := os.Rename(dir, dest); err != nil {
		if info, statErr := os.Stat(dest); statErr == nil && info.IsDir() {
			return nil
		}
		return err
	}
	return nil
}

type Launch struct {
	Attempt    string
	Spec       spec.Spec
	Allocation pool.Allocation
	Secrets    map[string]string
}

// Podman copies each secret into the container it creates, so the secret files and Podman secrets
// exist only while podman run does.
func launchScript(c Config, l Launch, server string) (string, io.Reader) {
	run := c.runDir(l.Attempt)
	r := podman.Run{Attempt: l.Attempt, Server: server, Image: l.Spec.Image, Command: l.Spec.Command, Allocation: l.Allocation, Sandbox: l.Spec.Sandbox, Outputs: path.Join(run, OutputsDir), CDISpecDir: c.cdiDir()}
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
	used := slices.AppendSeq([]string{c.imageMark(l.Spec.Image)}, maps.Values(r.Inputs))
	// A task running as a non-root image user gets a subordinate UID, which must still write its outputs.
	script := prelude() + "touch -c -- " + quoteAll(used) + "; mkdir -p -- " + ssh.Quote(r.Outputs) + "; chmod 777 -- " + ssh.Quote(r.Outputs) + "; "
	var stdin io.Reader
	if len(l.Secrets) > 0 {
		r.Secrets = slices.Sorted(maps.Keys(l.Secrets))
		var names []string
		for _, name := range r.Secrets {
			names = append(names, podman.SecretName(l.Attempt, name))
		}
		script += "d=$(mktemp -d " + ssh.Quote(path.Join(run, ".secrets.XXXXXX")) + "); s=" + ssh.Quote(strings.Join(names, " ")) +
			"; trap 'rm -rf -- \"$d\"; for x in $s; do podman --remote=false secret rm -- \"$x\" >/dev/null 2>&1 || :; done' EXIT; trap 'exit 129' HUP INT TERM; tar -x -f - -C \"$d\"; "
		var archive bytes.Buffer
		w := tar.NewWriter(&archive)
		for i, name := range r.Secrets {
			value := l.Secrets[name]
			_ = w.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(value))})
			_, _ = io.WriteString(w, value)
			script += "podman --remote=false secret create -- " + ssh.Quote(names[i]) + " \"$d\"/" + name + " >/dev/null; "
		}
		_ = w.Close()
		stdin = &archive
	}
	return script + "pm " + quoteAll(r.Argv()), stdin
}

// A repeated launch of one attempt fails on the container name.
func (h *Host) Launch(ctx context.Context, l Launch) (string, error) {
	script, stdin := launchScript(h.Config, l, h.server)
	data, err := h.output(ctx, script, stdin)
	if err != nil {
		h.forget()
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

func (h *Host) logs(ctx context.Context, attempt string, stdout, stderr io.Writer) error {
	return h.run.Run(ctx, prelude()+"pm logs -- "+ssh.Quote(podman.Name(attempt)), nil, stdout, stderr)
}

func (h *Host) outputs(ctx context.Context, attempt string, out io.Writer) error {
	// Keep find's failures and sub-second timestamps without relying on shell pipefail.
	script := "set -eu; cd -- " + ssh.Quote(path.Join(h.runDir(attempt), OutputsDir)) + "; l=$(mktemp); trap 'rm -f -- \"$l\"' EXIT; " +
		"find . -mindepth 1 -printf '%P\\0' > \"$l\"; tar -c -f - --format=posix --pax-option=delete=atime,delete=ctime --null --no-recursion --hard-dereference -T \"$l\""
	return h.run.Run(ctx, script, nil, out, nil)
}

// Prune removes the images Ordo loaded and the input trees it unpacked that keep does not name and no
// launch used for idle. Podman refuses to remove an image that a container still uses.
func (h *Host) Prune(ctx context.Context, keep []string, idle time.Duration) error {
	age := "+" + strconv.Itoa(int(idle.Minutes()))
	images, trees := ssh.Quote(h.imageDir()), ssh.Quote(path.Join(h.StateDir, "trees"))
	script := prelude() + "k=$(mktemp); trap 'rm -f -- \"$k\"' EXIT; cat > \"$k\"; " +
		"find " + images + " -mindepth 1 -maxdepth 1 -type f -mmin " + age + " -printf '%f\\n' | while IFS= read -r i; do grep -qxF -- \"$i\" \"$k\" && continue; " +
		"if pm image rm -- \"$i\" >/dev/null 2>&1 || ! pm image exists -- \"$i\"; then rm -f -- " + images + "/\"$i\"; fi; done; " +
		"find " + trees + " -mindepth 1 -maxdepth 1 -type d -mmin " + age + " ! -name '.*' -printf '%f\\n' | while IFS= read -r d; do grep -qxF -- \"$d\" \"$k\" || rm -rf -- " + trees + "/\"$d\"; done"
	err := h.run.Run(ctx, script, strings.NewReader(strings.Join(keep, "\n")+"\n"), nil, nil)
	h.forget()
	return err
}

// Callers remove only finished attempts, so a container that still runs is killed.
func (h *Host) Remove(ctx context.Context, attempt string) error {
	return h.run.Run(ctx, removeContainer(attempt)+"; rm -rf -- "+ssh.Quote(h.runDir(attempt)), nil, nil, nil)
}

// Release removes a finished attempt's container but keeps its run directory, whose outputs the job's
// next step starts from.
func (h *Host) Release(ctx context.Context, attempt string) error {
	return h.run.Run(ctx, removeContainer(attempt), nil, nil, nil)
}

// A launch killed before its EXIT trap ran leaves its Podman secrets behind.
func removeContainer(attempt string) string {
	return prelude() + "pm rm --force --ignore --time=0 -- " + ssh.Quote(podman.Name(attempt)) +
		"; for x in $(podman --remote=false secret ls --format '{{.Name}}'); do case $x in " + ssh.Quote(podman.SecretName(attempt, "")) + "*) podman --remote=false secret rm -- \"$x\" >/dev/null ;; esac; done"
}
