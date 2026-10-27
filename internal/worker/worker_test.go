package worker

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
)

// local runs scripts on this machine, standing in for an SSH worker.
type local struct{}

func (local) Run(ctx context.Context, script string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &ssh.ExitError{Code: exit.ExitCode()}
	}
	return err
}

const fakeSystemdRun = `#!/bin/sh
if [ "$1" = --help ]; then echo "  --expand-environment=BOOL"; exit 0; fi
printf '%s\0' systemd-run "$@" >> "$ORDO_FAKE_LOG"; printf '\036' >> "$ORDO_FAKE_LOG"
for a; do [ "$a" = --scope ] && scope=1; done
[ -n "${scope:-}" ] || exit 0
while [ "$1" != -- ]; do shift; done
shift
exec "$@"
`

const fakePodman = `#!/bin/sh
printf '%s\0' podman "$@" >> "$ORDO_FAKE_LOG"; printf '\036' >> "$ORDO_FAKE_LOG"
shift
case "$1" in
secret)
	case "$2" in
	create) for a; do :; done; cp "$a" "$ORDO_FAKE_LOG.secret"; stat -c %a "$a" > "$ORDO_FAKE_LOG.mode" ;;
	ls) printf 'ordo-a1-TOKEN\nordo-a10-TOKEN\nother\n' ;;
	esac ;;
run) echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
logs) echo "task stdout"; echo "task stderr" >&2 ;;
ps) echo '[{"Id":"x","State":"running","Labels":{"io.r3clab.ordo.attempt":"a1"},"StartedAt":1700000000}]' ;;
image)
	case "$2" in
	exists) exit "${ORDO_FAKE_IMAGE_EXISTS:-0}" ;;
	inspect) printf '[{"Id":"%s"}]' "$ORDO_FAKE_IMAGE" ;;
	esac ;;
load) cat > "$ORDO_FAKE_LOG.loaded" ;;
esac
`

func fakes(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, script := range map[string]string{"systemd-run": fakeSystemdRun, "podman": fakePodman, "systemctl": "#!/bin/sh\nexit 3\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	log := filepath.Join(dir, "log")
	t.Setenv("ORDO_FAKE_LOG", log)
	return log
}

func calls(t *testing.T, log, program string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var out [][]string
	for record := range strings.SplitSeq(strings.TrimSuffix(string(data), "\x1e"), "\x1e") {
		args := strings.Split(strings.TrimSuffix(record, "\x00"), "\x00")
		if args[0] == program {
			out = append(out, args[1:])
		}
	}
	return out
}

func testHost(t *testing.T) *Host {
	t.Helper()
	return New(Config{Host: "worker", StateDir: filepath.Join(t.TempDir(), "state"), CPUs: "0,64", MemoryMiB: 512}, local{}, "server1")
}

func TestLaunchQuotesArgumentsAndKeepsSecretsOffArgv(t *testing.T) {
	t.Chdir(t.TempDir())
	log := fakes(t)
	h := testHost(t)
	l := Launch{
		Attempt:    "a1",
		Spec:       spec.Spec{Image: "sha256:" + strings.Repeat("a", 64), Inputs: map[string]string{"data": strings.Repeat("c", 64)}, Task: spec.Task{Args: []string{"it's $HOME", "$(touch pwned)", "a\nb"}, Services: []string{"model"}, TimeoutSeconds: 10, Sandbox: spec.Sandbox{Env: map[string]string{"MODE": "eval"}}}},
		Allocation: pool.Allocation{CPUs: []int{0, 64}, MemoryMiB: 128},
		Secrets:    map[string]string{"TOKEN": "hunter2\nline 2\n"},
	}
	id, err := h.Launch(context.Background(), l)
	if err != nil || id != strings.Repeat("b", 64) {
		t.Fatalf("launch: %q, %v", id, err)
	}
	run := filepath.Join(h.StateDir, "runs", "a1")
	want := podman.Run{Attempt: "a1", Server: "server1", Image: l.Spec.Image, Args: l.Spec.Args, CPUs: []int{0, 64}, MemoryMiB: 128, Sandbox: l.Spec.Sandbox,
		Inputs: map[string]string{"data": filepath.Join(h.StateDir, "trees", strings.Repeat("c", 64))}, Services: map[string]string{"model": filepath.Join(h.StateDir, "services", "model")},
		Outputs: filepath.Join(run, "outputs"), CDISpecDir: filepath.Join(h.StateDir, "cdi"), Secrets: []string{"TOKEN"}, Timeout: 10 + backstop}
	podmanCalls := calls(t, log, "podman")
	if len(podmanCalls) != 3 || !slices.Equal(podmanCalls[1], append([]string{"--remote=false"}, want.Argv()...)) {
		t.Fatalf("podman argv = %q, want %q", podmanCalls, want.Argv())
	}
	if created, removed := podmanCalls[0], podmanCalls[2]; !slices.Equal(created[:5], []string{"--remote=false", "secret", "create", "--", "ordo-a1-TOKEN"}) || !slices.Equal(removed, []string{"--remote=false", "secret", "rm", "--", "ordo-a1-TOKEN"}) {
		t.Fatalf("secret must exist only around podman run: %q", podmanCalls)
	}
	if scope := calls(t, log, "systemd-run")[0]; !slices.Contains(scope, "--scope") || !slices.Contains(scope, "--expand-environment=no") {
		t.Fatalf("podman must run in its own scope without variable expansion: %q", scope)
	}
	if _, err := os.Stat("pwned"); err == nil {
		t.Fatal("an argument reached a shell")
	}
	if secret, err := os.ReadFile(log + ".secret"); err != nil || string(secret) != l.Secrets["TOKEN"] {
		t.Fatalf("secret = %q, %v", secret, err)
	}
	if mode, _ := os.ReadFile(log + ".mode"); strings.TrimSpace(string(mode)) != "600" {
		t.Fatalf("secret file mode %s", mode)
	}
	if left, _ := filepath.Glob(filepath.Join(run, ".secrets.*")); len(left) != 0 {
		t.Fatalf("secret files outlived podman run: %q", left)
	}
	if info, err := os.Stat(want.Outputs); err != nil || info.Mode().Perm() != 0o777 {
		t.Fatalf("outputs directory: %v", err)
	}
}

func TestRemoveDropsSecretsALaunchLeftBehind(t *testing.T) {
	log := fakes(t)
	h := testHost(t)
	if err := h.Remove(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	var removed []string
	for _, call := range calls(t, log, "podman") {
		if slices.Equal(call[1:3], []string{"secret", "rm"}) {
			removed = append(removed, call[len(call)-1])
		}
	}
	if !slices.Equal(removed, []string{"ordo-a1-TOKEN"}) {
		t.Fatalf("removed secrets %q", removed)
	}
}

func TestCheckLaunchBoundsTheRemoteCommand(t *testing.T) {
	c := Config{Host: "worker", StateDir: "/home/w/ordo", CPUs: "0-63", MemoryMiB: 512}
	s := spec.Spec{Image: "sha256:" + strings.Repeat("a", 64), Task: spec.Task{Resources: spec.Resources{Cores: 1, MemoryMiB: 128}, Args: []string{"x"}}}
	if err := c.CheckLaunch(s, nil); err != nil {
		t.Fatal(err)
	}
	s.Args = []string{strings.Repeat("'", ssh.MaxCommand/4)}
	if err := c.CheckLaunch(s, nil); err == nil {
		t.Fatal("accepted a launch command beyond the argument limit")
	}
}

func TestCollectStreamsLogsAndOutputs(t *testing.T) {
	fakes(t)
	h := testHost(t)
	outputs := filepath.Join(h.StateDir, "runs", "a1", "outputs")
	if err := os.MkdirAll(filepath.Join(outputs, "nested", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outputs, "nested", "value"), []byte("42"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(outputs, "nested", "value"), filepath.Join(outputs, "hard")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := h.Collect(context.Background(), "a1", dir); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{StdoutFile: "task stdout\n", StderrFile: "task stderr\n", "outputs/nested/value": "42", "outputs/hard": "42"} {
		if data, err := os.ReadFile(filepath.Join(dir, file)); err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v", file, data, err)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "outputs", "nested", "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty directory lost", err)
	}

	if err := os.Symlink("/etc/passwd", filepath.Join(outputs, "link")); err != nil {
		t.Fatal(err)
	}
	if err := h.Collect(context.Background(), "a1", t.TempDir()); err == nil {
		t.Fatal("collected a symbolic link")
	}
}

func TestEnsureInputUnpacksOnce(t *testing.T) {
	h := testHost(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "value"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	blobs := t.TempDir()
	digest, err := content.PutTree(blobs, src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(h.StateDir, "trees"), 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.EnsureInput(context.Background(), filepath.Join(blobs, digest), digest); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(h.StateDir, "trees", digest, "value")
	if info, err := os.Stat(file); err != nil || info.Mode().Perm()&0o044 != 0o044 {
		t.Fatalf("input must be readable by mapped container users: %v", err)
	}
}

func TestSeedMovesKeptOutputsOrUnpacksTheArchive(t *testing.T) {
	h := testHost(t)
	kept := filepath.Join(h.StateDir, "runs", "a", OutputsDir)
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, "klee.log"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.Seed(context.Background(), "b", "a", "/missing.tar"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(h.StateDir, "runs", "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the emptied run directory stayed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(h.StateDir, "runs", "b", OutputsDir, "klee.log")); err != nil || string(data) != "kept" {
		t.Fatalf("kept outputs not moved: %q %v", data, err)
	}
	src := t.TempDir()
	file := filepath.Join(src, "test000001.ktest")
	if err := os.WriteFile(file, []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Unix(1700000000, 250000000)
	if err := os.Chtimes(file, when, when); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "outputs.tar")
	if _, err := content.Archive(src, archive); err != nil {
		t.Fatal(err)
	}
	if err := h.Seed(context.Background(), "c", "a", archive); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(h.StateDir, "runs", "c", OutputsDir, "test000001.ktest"))
	if err != nil || !info.ModTime().Equal(when) || info.Mode().Perm() != 0o644 {
		t.Fatalf("unpacked outputs lost their time or mode: %v %v", info, err)
	}
}

func TestEnsureImageLoadsOnlyWhenMissing(t *testing.T) {
	log := fakes(t)
	h := testHost(t)
	id := "sha256:" + strings.Repeat("a", 64)
	archive := filepath.Join(t.TempDir(), "image.tar")
	if err := os.WriteFile(archive, []byte("archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := h.EnsureImage(context.Background(), archive, id); err != nil || len(calls(t, log, "podman")) != 1 {
			t.Fatal("present image reloaded or probed again", err)
		}
	}
	h = testHost(t)
	t.Setenv("ORDO_FAKE_IMAGE_EXISTS", "1")
	t.Setenv("ORDO_FAKE_IMAGE", "sha256:"+strings.Repeat("d", 64))
	if err := h.EnsureImage(context.Background(), archive, id); err == nil {
		t.Fatal("accepted a loaded image with another ID")
	}
	t.Setenv("ORDO_FAKE_IMAGE", id)
	if err := h.EnsureImage(context.Background(), archive, id); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(log + ".loaded"); string(data) != "archive" {
		t.Fatalf("archive was not streamed to podman load: %q", data)
	}
}

func TestPruneRemovesOnlyIdleUnneededImagesAndTrees(t *testing.T) {
	log := fakes(t)
	h := testHost(t)
	old := time.Now().Add(-25 * time.Hour)
	for _, name := range []string{"kept", "idle", "fresh"} {
		mark, tree := h.imageMark("sha256:"+name), h.treeDir(name)
		if err := errors.Join(os.MkdirAll(filepath.Dir(mark), 0o700), os.WriteFile(mark, nil, 0o600), os.MkdirAll(tree, 0o700)); err != nil {
			t.Fatal(err)
		}
		if name != "fresh" {
			if err := errors.Join(os.Chtimes(mark, old, old), os.Chtimes(tree, old, old)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := h.Prune(context.Background(), []string{"sha256:kept", "kept"}, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"kept": true, "idle": false, "fresh": true} {
		_, markErr := os.Stat(h.imageMark("sha256:" + name))
		_, treeErr := os.Stat(h.treeDir(name))
		if (markErr == nil) != want || (treeErr == nil) != want {
			t.Fatalf("%s: image mark %v, tree %v, want kept %v", name, markErr, treeErr, want)
		}
	}
	var removed [][]string
	for _, c := range calls(t, log, "podman") {
		if slices.Equal(c[1:3], []string{"image", "rm"}) {
			removed = append(removed, c)
		}
	}
	if len(removed) != 1 || removed[0][len(removed[0])-1] != "sha256:idle" {
		t.Fatalf("image removals = %q", removed)
	}
}

func TestPollAndStop(t *testing.T) {
	log := fakes(t)
	h := testHost(t)
	for _, attempt := range []string{"a1", "a2"} {
		if err := os.MkdirAll(filepath.Join(h.StateDir, "runs", attempt), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := h.Poll(context.Background())
	if err != nil || snap.BootID == "" || time.Since(snap.Now).Abs() > time.Minute || len(snap.Containers) != 1 || snap.Containers[0].Attempt() != "a1" || !slices.Equal(snap.Runs, []string{"a1", "a2"}) {
		t.Fatalf("poll: %+v, %v", snap, err)
	}
	if err := h.Stop(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	stops := calls(t, log, "systemd-run")
	stop := stops[len(stops)-1]
	if slices.Contains(stop, "--scope") || !slices.Contains(stop, "--unit=ordo-stop-a1") || !slices.Equal(stop[len(stop)-6:], []string{"podman", "--remote=false", "stop", "--time=10", "--", "ordo-a1"}) {
		t.Fatalf("stop must run asynchronously as a named service: %q", stop)
	}
}

func TestParseProbe(t *testing.T) {
	info := `{"host":{"os":"linux","cgroupVersion":"v2","cgroupControllers":["cpuset","memory"],"security":{"rootless":true}}}`
	text := "\n@@machine\nm1\n@@boot\nb1\n@@hostname\nvegeta\n@@arch\nx86_64\n@@online\n0-127\n" +
		"@@cpu\nmodel name\t: AMD EPYC 9554 64-Core Processor\n@@memory\nMemTotal:       1048576 kB\n@@linger\nyes\n@@podman\n" + info + "\n"
	p, err := parseProbe(sections(text), []int{0}, 512)
	if err != nil {
		t.Fatal(err)
	}
	h := p.Hardware
	if p.BootID != "b1" || h.MachineID != "m1" || h.Arch != "amd64" || h.CPUModel != "AMD EPYC 9554 64-Core Processor" || !slices.Equal(h.CPUs, []int{0}) {
		t.Fatalf("probe %+v", p)
	}
	cpu := []int{0}
	for _, bad := range []struct {
		name string
		cpus []int
		text string
	}{
		{"linger", cpu, strings.Replace(text, "@@linger\nyes", "@@linger\nno", 1)},
		{"offline", cpu, strings.Replace(text, "@@online\n0-127", "@@online\n1-127", 1)},
		{"memory budget", cpu, strings.Replace(text, "1048576 kB", "1024 kB", 1)},
	} {
		if _, err := parseProbe(sections(bad.text), bad.cpus, 512); err == nil {
			t.Fatalf("%s accepted", bad.name)
		}
	}
}

func TestGPUParsers(t *testing.T) {
	id := "GPU-11111111-1111-1111-1111-111111111111"
	gpus, err := parseGPUs([]byte("0, "+id+", NVIDIA L40S, 46068, Disabled\n1, GPU-other, X, 1, Disabled\n"), []string{id})
	if err != nil || len(gpus) != 1 || gpus[0].Model != "NVIDIA L40S" || gpus[0].MemoryMiB != 46068 {
		t.Fatalf("gpus %+v, %v", gpus, err)
	}
	if _, err := parseGPUs([]byte("0, "+id+", X, 1, Enabled\n"), []string{id}); err == nil {
		t.Fatal("accepted MIG")
	}
	if err := checkCDI([]byte(`{"kind":"nvidia.com/gpu","devices":[{"name":"`+id+`"}]}`), []string{id}); err != nil {
		t.Fatal(err)
	}
	if err := checkCDI([]byte(`{"kind":"nvidia.com/gpu","devices":[{"name":"all"}]}`), []string{id}); err == nil {
		t.Fatal("accepted an unrequested CDI device")
	}
	busy, err := parseBusy([]byte("# gpu pid\n# Idx #\n    0   1234  C\n    1      -  -\n"), []pool.GPU{{UUID: "a", Index: 0}, {UUID: "b", Index: 1}})
	if err != nil || !slices.Equal(busy, []string{"a"}) {
		t.Fatalf("busy %q, %v", busy, err)
	}
	if _, err := parseBusy([]byte("    0   -  -\n"), []pool.GPU{{UUID: "a", Index: 0}, {UUID: "b", Index: 1}}); err == nil {
		t.Fatal("accepted an incomplete process inventory")
	}
}

func TestConfigValidationAndServiceForward(t *testing.T) {
	good := Config{Host: "vegeta", StateDir: "/tmp/ordo-worker", CPUs: "62,63,126,127", MemoryMiB: 512}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Config){func(c *Config) { c.Host = "-oProxyCommand=bad" }, func(c *Config) { c.StateDir = "/tmp/../etc" }, func(c *Config) { c.StateDir = "/tmp/a:b" }, func(c *Config) { c.CPUs = "62,62" }, func(c *Config) { c.MemoryMiB = 0 }, func(c *Config) { c.Tags = []string{"a", "a"} }} {
		bad := good
		change(&bad)
		if err := bad.Validate(); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	c := Config{StateDir: "/home/worker/.local/state/ordo"}
	for address, want := range map[string]string{"127.0.0.1:8317": "127.0.0.1:8317", "[::1]:80": "[::1]:80", "gateway.lan:443": "gateway.lan:443"} {
		forward, err := c.ServiceForward("model", address)
		if err != nil || forward != "/home/worker/.local/state/ordo/services/model/socket:"+want {
			t.Fatalf("%s: %q, %v", address, forward, err)
		}
	}
	for name, address := range map[string]string{"model": "8317", "model ": "127.0.0.1:8317", "bad/name": "127.0.0.1:8317", strings.Repeat("x", 64): "127.0.0.1:8317", "port": "127.0.0.1:0", "named": "127.0.0.1:http", "option": "-oProxyCommand=x:22"} {
		if _, err := c.ServiceForward(name, address); err == nil {
			t.Fatalf("accepted %q=%q", name, address)
		}
	}
}
