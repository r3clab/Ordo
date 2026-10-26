package worker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func testAgent(t *testing.T) (*Agent, Config, Hardware) {
	t.Helper()
	cfg := Config{StateDir: filepath.Join(t.TempDir(), "agent"), CPUs: "0,1,64,65", MemoryMiB: 512}
	h := Hardware{MachineID: "test-machine", Arch: "amd64", Cores: [][]int{{0, 64}, {1, 65}}, MemoryMiB: 512}
	a, err := open(context.Background(), cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	return a, cfg, h
}
func testSpec() Spec {
	return Spec{Image: "sha256:" + strings.Repeat("a", 64), Resources: Resources{Cores: 1, MemoryMiB: 256}}
}

func TestReservationIdempotenceAndRestart(t *testing.T) {
	a, cfg, h := testAgent(t)
	first, err := a.Submit("one", testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Allocation.CPUs, []int{0, 64}) {
		t.Fatal(first)
	}
	again, err := a.Submit("one", testSpec(), nil)
	if err != nil || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("not idempotent: %v", err)
	}
	changed := testSpec()
	changed.Args = []string{"different"}
	if _, err := a.Submit("one", changed, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("accepted changed request: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a, err = open(context.Background(), cfg, h)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	second, err := a.Submit("two", testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second.Allocation.CPUs, []int{1, 65}) {
		t.Fatal("reservation lost across restart")
	}
	if _, err := a.Submit("three", testSpec(), nil); !errors.Is(err, ErrCapacity) {
		t.Fatalf("overcommitted: %v", err)
	}
}

func TestConcurrentReservations(t *testing.T) {
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var accepted []Attempt
	for _, id := range []string{"a", "b", "c", "d"} {
		wg.Go(func() {
			run, err := a.Submit(id, testSpec(), nil)
			if err == nil {
				mu.Lock()
				accepted = append(accepted, run)
				mu.Unlock()
			} else if !errors.Is(err, ErrCapacity) {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	if len(accepted) != 2 {
		t.Fatalf("admitted %d", len(accepted))
	}
	for _, cpu := range accepted[0].Allocation.CPUs {
		if slices.Contains(accepted[1].Allocation.CPUs, cpu) {
			t.Fatal("overlapping CPU reservations")
		}
	}
}

func TestChangedPoolCannotDiscardReservations(t *testing.T) {
	a, cfg, h := testAgent(t)
	if _, err := a.Submit("one", testSpec(), nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Hardware){func(h *Hardware) { h.Cores = [][]int{{1, 65}} }, func(h *Hardware) { h.MemoryMiB = 128 }, func(h *Hardware) { h.MachineID = "other-machine" }, func(h *Hardware) { h.Cores = [][]int{{0, 1, 64, 65}} }} {
		modified := h
		change(&modified)
		other, err := open(context.Background(), cfg, modified)
		if err == nil {
			_ = other.Close()
			t.Fatal("discarded or changed an active reservation")
		}
	}
}

func TestWorkerHTTPValidation(t *testing.T) {
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	handler := a.handler()
	for _, body := range []string{`{}`, `{"unknown":1}`, `{} {}`, `not JSON`} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPut, "/v1/attempts/id", strings.NewReader(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("accepted invalid request: %d %s", response.Code, response.Body)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/attempts/missing", nil))
	if response.Code != http.StatusNotFound {
		t.Fatal(response.Code)
	}
	if _, err := io.ReadAll(response.Result().Body); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryAccounting(t *testing.T) {
	h := Hardware{Cores: [][]int{{0, 64}, {1, 65}}, MemoryMiB: 512}
	if _, err := allocate(h, Resources{Cores: 1, MemoryMiB: 300}, []Allocation{{CPUs: []int{0, 64}, MemoryMiB: 256}}); !errors.Is(err, ErrCapacity) {
		t.Fatalf("memory overcommit: %v", err)
	}
}
