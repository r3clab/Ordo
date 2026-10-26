package worker

import (
	"context"
	"encoding/json"
	"github.com/r3clab/Ordo/internal/content"
	"github.com/r3clab/Ordo/internal/task"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestContentTransfer(t *testing.T) {
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	server := httptest.NewServer(a.handler())
	defer server.Close()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}}}
	defer client.CloseIdleConnections()
	root := t.TempDir()
	src := filepath.Join(root, "input")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "value"), []byte("immutable"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(root, "input.tar")
	hash, err := content.Archive(src, archive)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EnsureInput(context.Background(), client, archive, hash); err != nil {
			t.Fatal(err)
		}
	}
	spec := testSpec()
	spec.Inputs = map[string]string{"sample": hash}
	run, err := a.Submit("with-input", spec)
	if err != nil {
		t.Fatal(err)
	}
	r := task.Record{Request: task.Request{ExportOutputs: true}, State: "finished", ExitCode: new(0), OutputsSHA256: hash}
	dir := a.runDir(run.ID)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(r)
	for name, body := range map[string][]byte{"run.json": data, "stdout.log": {}, "stderr.log": {}} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := content.Archive(src, filepath.Join(dir, "outputs.tar")); err != nil {
		t.Fatal(err)
	}
	run.State = "finished"
	run.Result = &r
	a.attempts[run.ID] = run
	dest := filepath.Join(root, "download")
	if err := FetchResult(context.Background(), client, run.ID, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "outputs/value"))
	if err != nil || string(got) != "immutable" {
		t.Fatal(err)
	}
	if err := FetchResult(context.Background(), client, run.ID, dest); err == nil {
		t.Fatal("overwrote result directory")
	}
}
