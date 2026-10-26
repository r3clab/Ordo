package server

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/worker"
)

func TestDispatchResponseLossAndServerRestartDoNotDuplicate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "server")
	workers := []worker.Remote{{Host: "worker"}}
	s, err := Open(root, workers, nil, image.Tools{})
	if err != nil {
		t.Fatal(err)
	}
	var accepted *worker.Attempt
	puts := 0
	client := fakeWorker(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/blobs/"):
			w.WriteHeader(200)
		case r.URL.Path == "/v1/images":
			w.WriteHeader(200)
		case r.Method == http.MethodPut:
			puts++
			var submission worker.Submission
			if err := json.NewDecoder(r.Body).Decode(&submission); err != nil {
				t.Error(err)
			}
			accepted = &worker.Attempt{ID: strings.TrimPrefix(r.URL.Path, "/v1/attempts/"), Spec: submission.Spec, State: "running"}
			http.Error(w, "lost response after acceptance", 500)
		case accepted != nil:
			worker.WriteJSON(w, http.StatusOK, accepted)
		default:
			http.NotFound(w, r)
		}
	})
	j := Job{ID: "one", Spec: worker.Spec{Image: "sha256:" + strings.Repeat("a", 64), Resources: worker.Resources{Cores: 1, MemoryMiB: 128}}, Archive: strings.Repeat("b", 64), State: "queued", CreatedAt: time.Now()}
	if err := os.Mkdir(filepath.Join(root, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blobs", j.Archive), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.save(j); err != nil {
		t.Fatal(err)
	}
	info := worker.Info{ID: "agent", Hardware: worker.Hardware{Cores: [][]int{{0, 64}}, MemoryMiB: 128}}
	if err := s.reconcile(context.Background(), j, "worker", &info, client); err == nil {
		t.Fatal("expected response failure")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, workers, nil, image.Tools{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	j = s.List()[0]
	if len(j.Attempts) != 1 || j.State != "dispatching" {
		t.Fatal("dispatch not durable")
	}
	if err := s.reconcile(context.Background(), j, "worker", &info, client); err != nil {
		t.Fatal(err)
	}
	j = s.List()[0]
	if puts != 1 || j.State != "running" || j.Attempts[0].ID != accepted.ID {
		t.Fatal("duplicated execution")
	}
	info.ID = "replacement"
	if err := s.reconcile(context.Background(), j, "worker", &info, client); err != nil {
		t.Fatal(err)
	}
	if s.List()[0].State != "blocked" || puts != 1 {
		t.Fatal("silently adopted replacement agent")
	}
}

func fakeWorker(t *testing.T, handler http.HandlerFunc) *http.Client {
	remote := httptest.NewServer(handler)
	t.Cleanup(remote.Close)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", remote.Listener.Addr().String())
	}}}
	t.Cleanup(client.CloseIdleConnections)
	return client
}
