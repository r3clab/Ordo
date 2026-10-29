package ssh

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__ssh" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if err := Supervise(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

// Only bootstrap executes a shell. Forwarding is a local control request, not another session.
const fakeSSH = `#!/bin/sh
test -z "$FAKE_SSH_LOG" || echo "$*" >> "$FAKE_SSH_LOG"
for last; do :; done
case " $* " in
*" -O forward "*) exit 0 ;;
esac
exec /bin/sh -c "$last"
`

func installFakeSSH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(fakeSSH), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

func helper(t *testing.T) (string, []byte) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "ordo")
	data := []byte("#!/bin/sh\n[ \"$1\" = worker ] || exit 1\nread init\nprintf '/tmp/ordo-test/api\\n'\ncat >/dev/null\n")
	if err := os.WriteFile(file, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return file, data
}

func TestBootstrapCachesAnExactExecutableAndKeepsOneSession(t *testing.T) {
	installFakeSSH(t)
	binary, data := helper(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	selectBinary := func(arch string) (string, error) {
		if arch != "x86_64" && arch != "aarch64" {
			t.Errorf("architecture %q", arch)
		}
		return binary, nil
	}
	if _, err := Dial(ctx, "-oProxyCommand=x", "", nil, selectBinary); err == nil {
		t.Fatal("accepted option injection")
	}
	cache := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "ordo", "helpers", fmt.Sprintf("%x", sha256.Sum256(data)))
	var first time.Time
	for i := range 2 {
		m, err := Dial(ctx, "worker", "", map[string]string{"server": "test"}, selectBinary)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(cache)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("cached helper: %v, %v", info, err)
		}
		if i == 0 {
			first = info.ModTime()
		} else if !first.Equal(info.ModTime()) {
			t.Fatal("cached helper was uploaded again")
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Dir(m.Socket)); !os.IsNotExist(err) {
			t.Fatalf("local sockets survived Close: %v", err)
		}
	}
	// A corrupted cache entry is replaced, not executed.
	if err := os.WriteFile(cache, []byte("corrupt"), 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := Dial(ctx, "worker", "", nil, selectBinary)
	if err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	got, err := os.ReadFile(cache)
	if err != nil || string(got) != string(data) {
		t.Fatal("corrupted helper was reused", err)
	}
}

func TestConfigFileReachesBootstrapForwardsAndHome(t *testing.T) {
	installFakeSSH(t)
	binary, _ := helper(t)
	dir := t.TempDir()
	config, log := filepath.Join(dir, "ssh_config"), filepath.Join(dir, "log")
	t.Setenv("FAKE_SSH_LOG", log)
	ctx := context.Background()
	m, err := Dial(ctx, "worker", config, nil, func(string) (string, error) { return binary, nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Forward(ctx, "/s:127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := Home(ctx, "worker", config); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	// The bootstrap script contains newlines; only invocations start with the config option.
	var calls []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "-F ") {
			calls = append(calls, line)
		}
	}
	if len(calls) != 4 {
		t.Fatalf("expected bootstrap, two forwards and home: %q", calls)
	}
	for _, call := range calls {
		if !strings.HasPrefix(call, "-F "+config+" ") {
			t.Fatalf("lost config: %q", call)
		}
	}
	if strings.Count(string(data), " -M ") != 1 {
		t.Fatal("more than one master")
	}
}
