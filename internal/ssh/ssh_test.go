package ssh

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSSH runs the remote command locally and keeps a master alive until its stdin closes.
const fakeSSH = `#!/bin/sh
for last; do :; done
case " $* " in
*" -O check "*) test -e "$2" && exit 0; exit 1 ;;
*" -M "*) socket=$3; : > "$socket"; cat >/dev/null; rm -f "$socket"; exit 0 ;;
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
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunClassifiesRemoteAndTransportFailures(t *testing.T) {
	installFakeSSH(t)
	ctx := context.Background()
	if _, err := Dial(ctx, "-oProxyCommand=x"); err == nil {
		t.Fatal("allowed SSH option injection")
	}
	m, err := Dial(ctx, "worker")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := m.Run(ctx, `printf '%s' "$1" | cat; read line; printf ' %s' "$line"`, strings.NewReader("in\n"), &out, nil); err != nil || out.String() != " in" {
		t.Fatalf("run: %q, %v", out.String(), err)
	}
	out.Reset()
	if err := m.Run(ctx, "printf 'a'\\''b $x'", nil, &out, nil); err != nil || out.String() != "a'b $x" {
		t.Fatalf("quoting: %q, %v", out.String(), err)
	}
	var exit *ExitError
	if err := m.Run(ctx, "echo broken >&2; exit 3", nil, nil, nil); !errors.As(err, &exit) || exit.Code != 3 || exit.Stderr != "broken" || errors.Is(err, ErrTransport) {
		t.Fatalf("remote failure: %v", err)
	}
	if err := m.Run(ctx, "exit 255", nil, nil, nil); !errors.Is(err, ErrTransport) {
		t.Fatalf("ssh's own exit status must be a transport failure: %v", err)
	}
	if err := m.Run(ctx, strings.Repeat("x", MaxCommand), nil, nil, nil); err == nil || errors.Is(err, ErrTransport) {
		t.Fatalf("oversized script: %v", err)
	}
	broken := errors.New("disk full")
	if err := m.Run(ctx, "yes | head -c 1048576", nil, failingWriter{broken}, nil); !errors.Is(err, broken) || errors.Is(err, ErrTransport) {
		t.Fatalf("a local write failure is not the connection's: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Run(ctx, "true", nil, nil, nil); !errors.Is(err, ErrTransport) {
		t.Fatalf("closed master must be a transport failure: %v", err)
	}
}
