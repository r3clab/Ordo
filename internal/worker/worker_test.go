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
run)
	for a; do
		[ "${prev:-}" = --env-file ] && cp "$a" "$ORDO_FAKE_LOG.env" && stat -c %a "$a" > "$ORDO_FAKE_LOG.mode"
		prev=$a
	done
	echo bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ;;
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
		Secrets:    map[string]string{"TOKEN": "hunter2"},
	}
	id, err := h.Launch(context.Background(), l)
	if err != nil || id != strings.Repeat("b", 64) {
		t.Fatalf("launch: %q, %v", id, err)
	}
	run := filepath.Join(h.StateDir, "runs", "a1")
	want := podman.Run{Attempt: "a1", Server: "server1", Image: l.Spec.Image, Args: l.Spec.Args, CPUs: []int{0, 64}, MemoryMiB: 128, Sandbox: l.Spec.Sandbox,
		Inputs: map[string]string{"data": filepath.Join(h.StateDir, "trees", strings.Repeat("c", 64))}, Services: map[string]string{"model": filepath.Join(h.StateDir, "services", "model")},
		Outputs: filepath.Join(run, "outputs"), CDISpecDir: filepath.Join(h.StateDir, "cdi"), EnvFile: filepath.Join(run, ".secrets.env"), Timeout: 10 + backstop}
	podmanCalls := calls(t, log, "podman")
	if len(podmanCalls) != 1 || !slices.Equal(podmanCalls[0], append([]string{"--remote=false"}, want.Argv()...)) {
		t.Fatalf("podman argv = %q, want %q", podmanCalls, want.Argv())
	}
	if scope := calls(t, log, "systemd-run")[0]; !slices.Contains(scope, "--scope") || !slices.Contains(scope, "--expand-environment=no") {
		t.Fatalf("podman must run in its own scope without variable expansion: %q", scope)
	}
	if _, err := os.Stat("pwned"); err == nil {
		t.Fatal("an argument reached a shell")
	}
	if env, err := os.ReadFile(log + ".env"); err != nil || string(env) != "TOKEN=hunter2\n" {
		t.Fatalf("env file = %q, %v", env, err)
	}
	if mode, _ := os.ReadFile(log + ".mode"); strings.TrimSpace(string(mode)) != "600" {
		t.Fatalf("env file mode %s", mode)
	}
	if _, err := os.Stat(want.EnvFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("secrets survived launch")
	}
	if info, err := os.Stat(want.Outputs); err != nil || info.Mode().Perm() != 0o777 {
		t.Fatalf("outputs directory: %v", err)
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

func TestPollAndStop(t *testing.T) {
	log := fakes(t)
	h := testHost(t)
	snap, err := h.Poll(context.Background())
	if err != nil || snap.BootID == "" || time.Since(snap.Now).Abs() > time.Minute || len(snap.Containers) != 1 || snap.Containers[0].Attempt() != "a1" {
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
	text := "\n@@machine\nm1\n@@boot\nb1\n@@hostname\nvegeta\n@@arch\nx86_64\n@@online\n0-127\n@@siblings 0\n0,64\n@@siblings 64\n0,64\n" +
		"@@cpu\nmodel name\t: AMD EPYC 9554 64-Core Processor\n@@memory\nMemTotal:       1048576 kB\n@@linger\nyes\n@@podman\n" + info + "\n"
	p, err := parseProbe(sections(text), []int{0, 64}, 512)
	if err != nil {
		t.Fatal(err)
	}
	h := p.Hardware
	if p.BootID != "b1" || h.MachineID != "m1" || h.Arch != "amd64" || h.CPUModel != "AMD EPYC 9554 64-Core Processor" || len(h.Cores) != 1 || !slices.Equal(h.Cores[0], []int{0, 64}) {
		t.Fatalf("probe %+v", p)
	}
	pair := []int{0, 64}
	for _, bad := range []struct {
		name string
		cpus []int
		text string
	}{
		{"partial core", []int{0}, text},
		{"linger", pair, strings.Replace(text, "@@linger\nyes", "@@linger\nno", 1)},
		{"offline", pair, strings.Replace(text, "@@online\n0-127", "@@online\n1-127", 1)},
		{"memory budget", pair, strings.Replace(text, "1048576 kB", "1024 kB", 1)},
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
