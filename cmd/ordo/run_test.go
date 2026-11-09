package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/server"
)

type fakeRun struct {
	t          *testing.T
	mu         sync.Mutex
	line       map[string]json.RawMessage
	refuse     string
	job        func(served int) server.Job
	logs       map[string]string
	cut        map[string]int
	offsets    []string
	served     int
	unanswered int
	logStatus  int
	cancelled  atomic.Bool
	hold       bool
}

func (f *fakeRun) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/batches":
		var line map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&line); err != nil {
			f.t.Error(err)
		}
		f.mu.Lock()
		f.line = line
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(server.BatchResult{Line: 1, ID: "j1", Error: f.refuse})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/jobs/j1/cancel":
		f.cancelled.Store(true)
		_, _ = io.WriteString(w, "{}")
	case r.URL.Path == "/v1/jobs/j1":
		f.mu.Lock()
		f.unanswered--
		down, j := f.unanswered >= 0, f.job(f.served)
		f.mu.Unlock()
		if down {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		j.ID = "j1"
		_ = json.NewEncoder(w).Encode(j)
	case strings.HasPrefix(r.URL.Path, "/v1/jobs/j1/logs/"):
		q := r.URL.Query()
		key := q.Get("attempt") + "/" + strings.TrimPrefix(r.URL.Path, "/v1/jobs/j1/logs/")
		offset, _ := strconv.Atoi(q.Get("offset"))
		f.mu.Lock()
		f.offsets = append(f.offsets, key+"@"+q.Get("offset"))
		log, cut, cutting := f.logs[key], f.cut[key], false
		if _, ok := f.cut[key]; ok {
			delete(f.cut, key)
			cutting = true
		}
		f.mu.Unlock()
		switch {
		case f.hold:
			<-r.Context().Done()
			return
		case f.logStatus != 0:
			http.Error(w, "log is unavailable", f.logStatus)
			return
		}
		if cutting {
			_, _ = io.WriteString(w, log[offset:cut])
			if err := http.NewResponseController(w).Flush(); err != nil {
				f.t.Error(err)
			}
			panic(http.ErrAbortHandler)
		}
		_, _ = io.WriteString(w, log[offset:])
		f.mu.Lock()
		f.served++
		f.mu.Unlock()
	default:
		http.NotFound(w, r)
	}
}

func runFake(t *testing.T, f *fakeRun, args ...string) (stdout, errout string, err error) {
	t.Helper()
	f.t = t
	root := t.TempDir()
	serveSocket(t, root, f)
	poll := pollInterval
	pollInterval = time.Millisecond
	t.Cleanup(func() { pollInterval = poll; stderr = os.Stderr })
	var out, errs bytes.Buffer
	stderr = &errs
	err = run(context.Background(), append([]string{"run", "--state", root}, args...), &out)
	return out.String(), errs.String(), err
}

func result(code int) *server.Result { return &server.Result{ExitCode: code} }

func TestRunStreamsEachAttemptAndExitsWithItsStatus(t *testing.T) {
	retried := server.Attempt{ID: "a0", ContainerID: "c0", ResultDir: "/r/a0", Result: result(1), Failure: server.TaskFailure}
	const status = 7
	f := &fakeRun{
		logs:       map[string]string{"0/stdout": "first\n", "0/stderr": "oops\n", "1/stdout": "partial\n", "1/stderr": ""},
		cut:        map[string]int{"1/stdout": 3},
		unanswered: 2,
		job: func(served int) server.Job {
			running := server.Attempt{ID: "a1", ContainerID: "c1"}
			if served < 4 {
				return server.Job{State: server.Running, Attempts: []server.Attempt{retried, running}}
			}
			running.ResultDir, running.Result, running.Failure = "/r/a1", result(status), server.TaskFailure
			return server.Job{State: server.Failed, Attempts: []server.Attempt{retried, running}}
		},
	}
	dir := t.TempDir()
	t.Chdir(dir)
	stdout, errout, err := runFake(t, f, "--job", `{"inputs":{"data":"data"},"tags":["gpu"]}`, "docker://registry:5000/eval:v1", "sh", "-c", "cat /src/*.diff")
	if exitStatus(err) != status || err.Error() != "" {
		t.Fatalf("run must exit with the container's status and say nothing more: %d %q", exitStatus(err), err)
	}
	if stdout != "first\npartial\n" || !strings.HasPrefix(errout, "oops\nordo: retrying, as attempt 1 exited with status 1\n") {
		t.Fatalf("stdout %q, stderr %q", stdout, errout)
	}
	if !strings.Contains(strings.Join(f.offsets, " "), "1/stdout@3") {
		t.Fatalf("a broken follow must resume where it left off: %q", f.offsets)
	}
	want := map[string]string{
		"image":     `"docker://registry:5000/eval:v1"`,
		"args":      `["sh","-c","cat /src/*.diff"]`,
		"resources": `{"cores":1,"memory_mib":1024}`,
		"inputs":    `{"data":"` + filepath.Join(dir, "data") + `"}`,
		"tags":      `["gpu"]`,
	}
	if len(f.line) != len(want) {
		t.Fatalf("line %s", f.line)
	}
	for name, value := range want {
		if string(f.line[name]) != value {
			t.Fatalf("line %s = %s, want %s", name, f.line[name], value)
		}
	}
}

func TestRunReportsOrdoFailuresApart(t *testing.T) {
	ended := func(state server.State, attempts ...server.Attempt) func(int) server.Job {
		return func(int) server.Job { return server.Job{State: state, Error: "why", Attempts: attempts} }
	}
	timedOut := server.Attempt{ID: "a0", ResultDir: "/r", Result: result(0), StopRequested: server.Timeout, Failure: server.TaskFailure}
	for name, test := range map[string]struct {
		f       *fakeRun
		cancels bool
	}{
		"refused":    {f: &fakeRun{refuse: "no matching worker"}},
		"cancelled":  {f: &fakeRun{job: ended(server.Cancelled)}},
		"lost":       {f: &fakeRun{job: ended(server.Failed, server.Attempt{ID: "a0", Failure: server.InfrastructureFailure})}},
		"timed out":  {f: &fakeRun{job: ended(server.Failed, timedOut)}},
		"blocked":    {f: &fakeRun{job: ended(server.Blocked)}, cancels: true},
		"unreadable": {f: &fakeRun{job: ended(server.Running, server.Attempt{ID: "a0", ContainerID: "c0"}), logStatus: http.StatusInternalServerError}, cancels: true},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := runFake(t, test.f, "/src#image")
			if exitStatus(err) != exitRunFailed || err.Error() == "" {
				t.Fatalf("status %d: %v", exitStatus(err), err)
			}
			if test.f.cancelled.Load() != test.cancels {
				t.Fatal("run must cancel a job it leaves unfinished, and only such a job")
			}
		})
	}
	if _, _, err := runFake(t, &fakeRun{job: ended(server.Succeeded, server.Attempt{ID: "a0", ResultDir: "/r", Result: result(0)})}, "/src#image"); err != nil {
		t.Fatalf("a succeeded job must exit 0: %v", err)
	}
}

func TestRunCancelsItsJobWhenInterrupted(t *testing.T) {
	f := &fakeRun{t: t, hold: true, job: func(int) server.Job {
		return server.Job{State: server.Running, Attempts: []server.Attempt{{ID: "a0", ContainerID: "c0"}}}
	}}
	root := t.TempDir()
	serveSocket(t, root, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"run", "--state", root, "/src#image"}, io.Discard) }()
	for {
		f.mu.Lock()
		following := len(f.offsets) > 0
		f.mu.Unlock()
		if following {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; exitStatus(err) != exitInterrupted || !f.cancelled.Load() {
		t.Fatalf("status %d, cancelled %v", exitStatus(err), f.cancelled.Load())
	}
}

func TestRunMisuseExitsApartFromTheContainer(t *testing.T) {
	for _, args := range [][]string{{"run", "--state", t.TempDir()}, {"run", "/src#image"}, {"run", "--bogus"}} {
		if err := run(context.Background(), args, io.Discard); exitStatus(err) != exitRunFailed {
			t.Errorf("%q: status %d: %v", args, exitStatus(err), err)
		}
	}
	if err := run(context.Background(), []string{"run", "--help"}, io.Discard); err != nil {
		t.Fatal("help is not a failure", err)
	}
}

func TestRunJobMustNotSetWhatRunDoes(t *testing.T) {
	for _, job := range []string{`{"out":"/x"}`, `{"steps":[]}`, `{"image":"/src#x"}`, `{"args":[]}`, `[]`, `null`} {
		if _, err := runLine(job, "/src#image", nil); err == nil {
			t.Errorf("accepted --job %s", job)
		}
	}
}
