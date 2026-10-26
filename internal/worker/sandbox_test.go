package worker

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/r3clab/Ordo/internal/task"
)

func TestServiceForward(t *testing.T) {
	r := Remote{StateDir: "/home/worker/.local/state/ordo"}
	for address, want := range map[string]string{"127.0.0.1:8317": "127.0.0.1:8317", "[::1]:80": "[::1]:80", "gateway.lan:443": "gateway.lan:443"} {
		forward, err := r.ServiceForward("model", address)
		if err != nil || forward != "/home/worker/.local/state/ordo/services/model/socket:"+want {
			t.Fatalf("%s: %q, %v", address, forward, err)
		}
	}
	for name, address := range map[string]string{"model": "8317", "model ": "127.0.0.1:8317", "bad/name": "127.0.0.1:8317", strings.Repeat("x", 64): "127.0.0.1:8317"} {
		if _, err := r.ServiceForward(name, address); err == nil {
			t.Fatalf("accepted %q=%q", name, address)
		}
	}
	for _, address := range []string{"127.0.0.1:0", "127.0.0.1:http", "-oProxyCommand=x:22", ":8317"} {
		if _, err := r.ServiceForward("model", address); err == nil {
			t.Fatalf("accepted %q", address)
		}
	}
}

func TestSecretsAreAdmittedApartAndDropped(t *testing.T) {
	a, cfg, h := testAgent(t)
	defer func() { _ = a.Close() }()
	spec := testSpec()
	spec.Secrets = []string{"TOKEN"}
	spec.Services = []string{"model"}
	for _, secrets := range []map[string]string{nil, {"OTHER": "x"}, {"TOKEN": "a\nb"}} {
		if _, err := a.Submit("bad", spec, secrets); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("accepted secrets %q: %v", secrets, err)
		}
	}
	run, err := a.Submit("one", spec, map[string]string{"TOKEN": "hunter2"})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	a.handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/attempts/one", nil))
	if strings.Contains(response.Body.String(), "hunter2") || !strings.Contains(response.Body.String(), `"TOKEN"`) {
		t.Fatalf("attempt must name secrets without values: %s", response.Body)
	}
	req := task.Request{Services: map[string]string{"model": a.serviceDir("model")}}
	if err := a.prepareLaunch(run, &req); err != nil || req.Secrets["TOKEN"] != "hunter2" {
		t.Fatalf("launch lacks secret values: %v", err)
	}
	if info, err := os.Stat(a.serviceDir("model")); err != nil || !info.IsDir() {
		t.Fatal("service directory not prepared before mount")
	}
	if err := a.Cancel("one", "budget spent"); err != nil {
		t.Fatal(err)
	}
	if got := a.attempts["one"]; got.State != "cancelled" || got.StopReason != "budget spent" {
		t.Fatalf("cancel reason lost: %+v", got)
	}
	var values map[string]string
	if err := a.store.Get("secret", "one", &values); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("terminal attempt kept its secrets: %v", err)
	}

	if _, err := a.Submit("live", spec, map[string]string{"TOKEN": "kept"}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Put("secret", "orphan", map[string]string{"TOKEN": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if a, err = open(context.Background(), cfg, h); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Get("secret", "orphan", &values); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("restart kept an orphaned secret: %v", err)
	}
	if err := a.store.Get("secret", "live", &values); err != nil || values["TOKEN"] != "kept" {
		t.Fatalf("restart dropped a live attempt's secret: %v", err)
	}
}

func TestStopKeepsResultAndFirstRequestWins(t *testing.T) {
	a, _, _ := testAgent(t)
	defer func() { _ = a.Close() }()
	run, err := a.Submit("run", testSpec(), nil)
	if err != nil {
		t.Fatal(err)
	}
	a.busy[run.ID] = true
	handler := a.handler()
	for _, c := range []struct {
		body string
		want int
	}{{`{"bogus":1}`, http.StatusBadRequest}, {`{"reason":"budget spent"}`, http.StatusOK}, {``, http.StatusOK}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/attempts/run/stop", strings.NewReader(c.body)))
		if response.Code != c.want {
			t.Fatalf("%q: %d %s", c.body, response.Code, response.Body)
		}
	}
	if err := a.Cancel(run.ID, "later"); err != nil {
		t.Fatal(err)
	}
	got := a.attempts[run.ID]
	if got.StopRequested != "stop" || got.StopReason != "budget spent" || got.Terminal() {
		t.Fatalf("stop request not kept: %+v", got)
	}
	got.State = "finished"
	if err := a.save(got); err != nil {
		t.Fatal(err)
	}
	if a.attempts[run.ID].State != "finished" {
		t.Fatal("stopped attempt reported as cancelled")
	}
}
