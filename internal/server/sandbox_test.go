package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/task"
	"github.com/r3clab/Ordo/internal/worker"
)

func TestSecretsServicesAndStop(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "image.tar")
	if err := os.WriteFile(archive, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, "state")
	workers := []worker.Remote{{Host: "worker", StateDir: "/home/worker/ordo"}}
	services := map[string]string{"model": "127.0.0.1:8317"}
	s, err := Open(state, workers, services, image.Tools{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := Request{ID: "agent", Image: image.Source{Archive: archive, ID: "sha256:" + strings.Repeat("a", 64)}, Resources: worker.Resources{Cores: 1, MemoryMiB: 128},
		Sandbox: task.Sandbox{Env: map[string]string{"MODE": "eval"}, Network: task.Egress, CapAdd: []string{"NET_ADMIN"}},
		Secrets: map[string]string{"TOKEN": "hunter2"}, Services: []string{"model"}, Retry: RetryPolicy{Task: 2}}
	unknown := req
	unknown.Services = []string{"other"}
	if _, err := s.Submit(ctx, unknown); err == nil {
		t.Fatal("accepted an unconfigured service")
	}
	j, err := s.Submit(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	resubmitted := req
	resubmitted.Secrets = map[string]string{"TOKEN": "rotated"}
	if _, err := s.Submit(ctx, resubmitted); err != nil {
		t.Fatal("secret values must not change job identity:", err)
	}
	listed, _ := json.Marshal(s.List())
	if strings.Contains(string(listed), "hunter2") || strings.Contains(string(listed), "rotated") || !slices.Equal(j.Spec.Secrets, []string{"TOKEN"}) {
		t.Fatalf("jobs must name secrets without values: %s", listed)
	}

	var accepted *worker.Attempt
	var submitted worker.Submission
	var stops []string
	client := fakeWorker(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/blobs/"), r.URL.Path == "/v1/images":
			w.WriteHeader(200)
		case r.Method == http.MethodPut:
			if err := worker.DecodeJSON(r.Body, &submitted); err != nil {
				t.Error(err)
			}
			accepted = &worker.Attempt{ID: strings.TrimPrefix(r.URL.Path, "/v1/attempts/"), Spec: submitted.Spec, State: "running"}
			worker.WriteJSON(w, http.StatusOK, accepted)
		case r.Method == http.MethodPost:
			reason, err := worker.DecodeReason(w, r)
			if err != nil {
				t.Error(err)
			}
			stops = append(stops, filepath.Base(r.URL.Path)+":"+reason)
			worker.WriteJSON(w, http.StatusOK, map[string]bool{"requested": true})
		case accepted != nil:
			worker.WriteJSON(w, http.StatusOK, accepted)
		default:
			http.NotFound(w, r)
		}
	})
	info := worker.Info{ID: "agent", Hardware: worker.Hardware{Cores: [][]int{{0, 64}}, MemoryMiB: 128}}
	if err := s.reconcile(ctx, j, workers[0], &info, client); err != nil {
		t.Fatal(err)
	}
	if submitted.Secrets["TOKEN"] != "hunter2" || !slices.Equal(submitted.Spec.Services, []string{"model"}) || submitted.Spec.Network != task.Egress {
		t.Fatalf("dispatch lost the sandbox or original secret: %+v", submitted)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(state, workers, nil, image.Tools{}); err == nil {
		_ = other.Close()
		t.Fatal("dropped a service an unfinished job uses")
	}
	if s, err = Open(state, workers, services, image.Tools{}); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	stale, _ := s.job("agent")
	if err := s.Stop("agent", "budget spent"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(stale); err != nil {
		t.Fatal(err)
	}
	j, _ = s.job("agent")
	if !j.StopRequested {
		t.Fatal("a stale copy dropped the stop")
	}
	if err := s.reconcile(ctx, j, workers[0], &info, client); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(stops, []string{"stop:budget spent"}) {
		t.Fatalf("stop not delivered: %q", stops)
	}
	j, _ = s.job("agent")
	if err := s.finish(j, "task"); err != nil {
		t.Fatal(err)
	}
	if j, _ = s.job("agent"); j.State != "failed" || j.StopReason != "budget spent" {
		t.Fatalf("stopped job retried or lost its reason: %+v", j)
	}
	var values map[string]string
	if err := s.store.Get("secret", "agent", &values); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("terminal job kept its secrets: %v", err)
	}

	queued := req
	queued.ID = "queued"
	if _, err := s.Submit(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop("queued", ""); err != nil {
		t.Fatal(err)
	}
	if j, _ := s.job("queued"); j.State != "cancelled" {
		t.Fatalf("stopping an unstarted job must cancel it: %s", j.State)
	}

	oob := req
	oob.ID = "oob"
	if j, err = s.Submit(ctx, oob); err != nil {
		t.Fatal(err)
	}
	if err := s.reconcile(ctx, j, workers[0], &info, client); err != nil {
		t.Fatal(err)
	}
	accepted.State, accepted.StopRequested, accepted.StopReason = "infrastructure_failed", "stop", "operator"
	j, _ = s.job("oob")
	if err := s.reconcile(ctx, j, workers[0], &info, client); err != nil {
		t.Fatal(err)
	}
	if j, _ = s.job("oob"); j.State != "failed" || !j.StopRequested || j.StopReason != "operator" {
		t.Fatalf("a stop sent to the worker directly must end the job: %+v", j)
	}

	live := req
	live.ID = "live"
	if _, err := s.Submit(ctx, live); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(state, workers, services, image.Tools{}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.Get("secret", "live", &values); err != nil || values["TOKEN"] != "hunter2" {
		t.Fatalf("restart dropped a live job's secret: %v", err)
	}
}
