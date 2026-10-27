package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/image"
	"github.com/r3clab/Ordo/internal/image/imagetest"
	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
	"github.com/r3clab/Ordo/internal/worker"
)

func testWorker(host string, tags ...string) worker.Config {
	return worker.Config{Host: host, StateDir: "/srv/ordo", CPUs: "0-3", MemoryMiB: 1024, Tags: tags}
}

var testImageID = "sha256:" + strings.Repeat("a", 64)

func testNix() *imagetest.Nix {
	return &imagetest.Nix{Output: "/nix/store/aaaa-image.json", Config: testImageID}
}

func testTools() image.Tools { return image.Tools{Exec: testNix().Exec} }

func handlerClient(t *testing.T, handler http.Handler) *http.Client {
	remote := httptest.NewServer(handler)
	t.Cleanup(remote.Close)
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", remote.Listener.Addr().String())
	}}}
	t.Cleanup(client.CloseIdleConnections)
	return client
}

type fakeContainer struct {
	podman.Container
	exit     int
	finished time.Time
}

// fakeHost is a worker with four single-thread cores whose containers run only when a test says so.
// Its methods lock mu, as a pass runs launches and collections beside it; a test reads and changes it
// only between ticks.
type fakeHost struct {
	mu         sync.Mutex
	server     string
	boot       string
	machine    string
	skew       time.Duration
	containers map[string]*fakeContainer
	launches   []worker.Launch
	stops      []string
	collects   int
	removed    []string
	// lose is returned by the next launch after it creates the container; refuse, before.
	lose, refuse error
	broken       error
	// upload fails the next EnsureImage.
	upload error
}

func newFakeHost(server string) *fakeHost {
	return &fakeHost{server: server, boot: "boot-1", machine: "machine-1", containers: map[string]*fakeContainer{}}
}

func (f *fakeHost) now() time.Time { return time.Now().Add(f.skew) }

func (f *fakeHost) add(attempt, server, state string) *fakeContainer {
	id := strings.Repeat(strconv.Itoa(len(f.containers)%10), 64)
	now := podman.Time{Time: f.now()}
	c := &fakeContainer{Container: podman.Container{ID: id, State: state, Labels: map[string]string{podman.AttemptLabel: attempt, podman.ServerLabel: server}, Created: now, StartedAt: now}}
	f.containers[attempt] = c
	return c
}

func (f *fakeHost) Probe(context.Context) (worker.Probe, error) {
	return worker.Probe{BootID: f.boot, Hardware: pool.Hardware{Hostname: "worker", MachineID: f.machine, Arch: "amd64", Cores: [][]int{{0}, {1}, {2}, {3}}, MemoryMiB: 1024}}, nil
}

func (f *fakeHost) Poll(context.Context) (worker.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := worker.Snapshot{BootID: f.boot, Now: f.now()}
	for _, c := range f.containers {
		snap.Containers = append(snap.Containers, c.Container)
	}
	return snap, nil
}

func (f *fakeHost) EnsureImage(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.upload
	f.upload = nil
	return err
}

func (f *fakeHost) EnsureInput(context.Context, string, string) error { return nil }

func (f *fakeHost) GPUBusy(context.Context, []pool.GPU) ([]string, error) { return nil, nil }

func (f *fakeHost) Launch(_ context.Context, l worker.Launch) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.refuse; err != nil {
		f.refuse = nil
		return "", err
	}
	if _, ok := f.containers[l.Attempt]; ok {
		return "", &ssh.ExitError{Code: 125, Stderr: "name is already in use"}
	}
	f.launches = append(f.launches, l)
	c := f.add(l.Attempt, f.server, podman.Running)
	err := f.lose
	f.lose = nil
	if err != nil {
		return "", err
	}
	return c.ID, nil
}

func (f *fakeHost) exit(attempt string, code int) {
	c := f.containers[attempt]
	c.State, c.exit, c.finished = podman.Exited, code, c.StartedAt.Add(time.Second)
}

func (f *fakeHost) Stop(_ context.Context, attempt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, attempt)
	f.exit(attempt, 137)
	return nil
}

func (f *fakeHost) Inspect(_ context.Context, attempt string) (podman.Inspection, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in podman.Inspection
	c, ok := f.containers[attempt]
	if !ok {
		return in, errors.New("no such container")
	}
	in.ID, in.Name, in.Image = c.ID, podman.Name(attempt), testImageID
	in.Config.Labels = c.Labels
	in.State.Status, in.State.ExitCode, in.State.StartedAt, in.State.FinishedAt = c.State, &c.exit, c.StartedAt.Time, c.finished
	return in, nil
}

func (f *fakeHost) Collect(_ context.Context, attempt, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.collects++
	if f.broken != nil {
		return f.broken
	}
	for name, data := range map[string]string{worker.StdoutFile: "out", worker.StderrFile: "err", worker.OutputsDir + "/score": attempt} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeHost) Remove(_ context.Context, attempt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, attempt)
	f.removed = append(f.removed, attempt)
	return nil
}

func open(t *testing.T, state string, workers []worker.Config, services map[string]string) *Server {
	t.Helper()
	s, err := Open(Config{Root: state, Workers: workers, Services: services, Tools: testTools()})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func request(id string) Request {
	return Request{ID: id, Image: image.Source("/src#image"), Task: spec.Task{Resources: spec.Resources{Cores: 1, MemoryMiB: 128}}}
}

func submit(t *testing.T, s *Server, r Request) {
	t.Helper()
	if _, err := s.Submit(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// tick waits for the pass's background steps, as the next pass would find them still busy.
func tick(s *Server, cfg worker.Config, h *fakeHost) error {
	probe, err := h.Probe(context.Background())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	bg := &steps{cancel: cancel}
	err = s.pass(ctx, cfg, h, &probe, bg)
	bg.wg.Wait()
	return cmp.Or(err, context.Cause(ctx))
}

func mustTick(t *testing.T, s *Server, cfg worker.Config, h *fakeHost) {
	t.Helper()
	if err := tick(s, cfg, h); err != nil {
		t.Fatal(err)
	}
}

// seed commits jobs as they are, which save would refuse for a job it does not know.
func seed(t *testing.T, s *Server, jobs ...Job) {
	t.Helper()
	s.write.Lock()
	defer s.write.Unlock()
	if err := s.commit(jobs...); err != nil {
		t.Fatal(err)
	}
}

func mustJob(t *testing.T, s *Server, id string) Job {
	t.Helper()
	j, ok, err := s.job(id)
	if !ok || err != nil {
		t.Fatalf("job %s missing: %v", id, err)
	}
	return j
}

// fixture is one server with one fake worker.
type fixture struct {
	s   *Server
	cfg worker.Config
	h   *fakeHost
}

func newFixture(t *testing.T) fixture {
	cfg := testWorker("worker")
	s := open(t, filepath.Join(t.TempDir(), "state"), []worker.Config{cfg}, nil)
	t.Cleanup(func() { _ = s.Close() })
	return fixture{s: s, cfg: cfg, h: newFakeHost(s.id)}
}

func (f fixture) tick(t *testing.T) {
	t.Helper()
	mustTick(t, f.s, f.cfg, f.h)
}

func TestLostLaunchResponseAndRestartDoNotDuplicate(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	workers := []worker.Config{testWorker("worker")}
	s := open(t, state, workers, nil)
	submit(t, s, request("one"))
	h := newFakeHost(s.id)
	h.lose = ssh.ErrTransport
	if err := tick(s, workers[0], h); !errors.Is(err, ssh.ErrTransport) {
		t.Fatalf("lost launch response: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, state, workers, nil)
	defer func() { _ = s.Close() }()
	if j := mustJob(t, s, "one"); len(j.Attempts) != 1 || j.State != Dispatching {
		t.Fatalf("dispatch not durable: %+v", j)
	}
	mustTick(t, s, workers[0], h)
	j := mustJob(t, s, "one")
	at := j.Attempts[0]
	if len(h.launches) != 1 || j.State != Running || at.ContainerID != h.containers[at.ID].ID {
		t.Fatalf("restart did not adopt the launched container: %d launches, %+v", len(h.launches), j)
	}

	h.exit(at.ID, 0)
	mustTick(t, s, workers[0], h)
	j = mustJob(t, s, "one")
	at = j.Attempts[0]
	if j.State != Succeeded || at.Result == nil || at.Result.ExitCode != 0 || len(at.Result.OutputsSHA256) != 64 || len(h.containers) != 0 {
		t.Fatalf("collection: %+v, containers %v", j, h.containers)
	}
	record, err := readRecord(at.ResultDir)
	if err != nil || record.Attempt != at.ID || record.Spec.Image != j.Spec.Image || record.OutputsSHA256 != at.Result.OutputsSHA256 {
		t.Fatalf("run.json %+v: %v", record, err)
	}
	if _, err := os.Stat(filepath.Join(at.ResultDir, OutputsArchive)); err != nil {
		t.Fatal(err)
	}
}

func TestRefusedLaunchRetriesAndChangedWorkerBlocks(t *testing.T) {
	f := newFixture(t)
	submit(t, f.s, request("job"))
	f.h.refuse = &ssh.ExitError{Code: 125, Stderr: "image not known"}
	f.tick(t)
	j := mustJob(t, f.s, "job")
	if j.State != Queued || j.Attempts[0].Failure != InfrastructureFailure || !slices.Contains(f.h.removed, j.Attempts[0].ID) {
		t.Fatalf("refused launch must requeue as an infrastructure failure: %+v", j)
	}
	f.tick(t)
	if j = mustJob(t, f.s, "job"); j.State != Running || len(j.Attempts) != 2 {
		t.Fatalf("retry not launched: %+v", j)
	}

	f.h.machine = "machine-2"
	f.tick(t)
	f.h.machine = "machine-1"
	f.tick(t)
	if j = mustJob(t, f.s, "job"); j.State != Blocked || len(f.h.launches) != 1 {
		t.Fatalf("a changed worker must block the job until it is cancelled: %+v", j)
	}
	if err := f.s.Cancel("job", false, ""); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j = mustJob(t, f.s, "job"); j.State != Cancelled || len(f.h.containers) != 0 {
		t.Fatalf("cancel must release a blocked job and sweep its container: %+v, %v", j, f.h.containers)
	}
}

func TestRetryReleasesTheFinishedAttempt(t *testing.T) {
	f := newFixture(t)
	req := request("whole")
	req.Resources.Cores, req.Retry.Task = 4, 1
	submit(t, f.s, req)
	f.tick(t)
	first := mustJob(t, f.s, "whole").Attempts[0]
	f.h.exit(first.ID, 1)
	f.tick(t)
	if j := mustJob(t, f.s, "whole"); j.State != Queued || j.Attempts[0].Failure != TaskFailure {
		t.Fatalf("failed task not requeued: %+v", j)
	}
	f.tick(t)
	if j := mustJob(t, f.s, "whole"); j.State != Running || len(j.Attempts) != 2 {
		t.Fatalf("the finished attempt still holds the worker: %+v", j)
	}
}

func TestFailedCollectionBlocksOnce(t *testing.T) {
	f := newFixture(t)
	submit(t, f.s, request("job"))
	f.tick(t)
	at := mustJob(t, f.s, "job").Attempts[0]
	f.h.exit(at.ID, 0)
	f.h.broken = errors.New("archive links and special files are unsupported")
	f.tick(t)
	f.tick(t)
	if j := mustJob(t, f.s, "job"); j.State != Blocked || f.h.collects != 1 || !strings.Contains(j.Error, "special files") {
		t.Fatalf("collection retried or not blocked: %d collects, %+v", f.h.collects, j)
	}
	if err := f.s.Cancel("job", false, "bad outputs"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j := mustJob(t, f.s, "job"); j.State != Cancelled || len(f.h.containers) != 0 {
		t.Fatalf("cancel must release the job and remove its container: %+v", j)
	}
}

func TestTagsSelectWorkers(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(Config{Root: filepath.Join(root, "bad"), Workers: []worker.Config{testWorker("a", "l40s", "l40s")}}); err == nil {
		t.Fatal("accepted a duplicate worker tag")
	}
	workers := []worker.Config{testWorker("plain"), testWorker("a", "l40s"), testWorker("b", "fast")}
	state := filepath.Join(root, "state")
	s := open(t, state, workers, nil)
	defer func() { _ = s.Close() }()
	req := request("tagged")
	req.Tags = []string{"l40s", "fast"}
	if _, err := s.Submit(context.Background(), req); err == nil {
		t.Fatal("accepted tags that no single worker has")
	}
	req.Tags = []string{"l40s"}
	submit(t, s, req)
	for _, cfg := range workers {
		h := newFakeHost(s.id)
		mustTick(t, s, cfg, h)
		want := 0
		if cfg.Host == "a" {
			want = 1
		}
		if len(h.launches) != want {
			t.Fatalf("worker %s launched %d, want %d", cfg.Host, len(h.launches), want)
		}
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(Config{Root: state, Workers: []worker.Config{testWorker("plain"), testWorker("a"), testWorker("b", "fast")}}); err == nil {
		_ = other.Close()
		t.Fatal("dropped a tag an unfinished job uses")
	}
	s = open(t, state, workers, nil)
}

func TestSecretsServicesAndStop(t *testing.T) {
	ctx := context.Background()
	config := Config{Root: filepath.Join(t.TempDir(), "state"), Workers: []worker.Config{testWorker("worker")}, Services: map[string]string{"model": "127.0.0.1:8317"}, Secrets: map[string]string{"TOKEN": "hunter2"}, Tools: testTools()}
	workers := config.Workers
	broken := config
	broken.Secrets = map[string]string{"TOKEN": "a\nb"}
	if _, err := Open(broken); err == nil {
		t.Fatal("accepted a secret the env file cannot carry")
	}
	s, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	req := request("agent")
	req.Sandbox = spec.Sandbox{Env: map[string]string{"MODE": "eval"}, Network: spec.Egress, CapAdd: []string{"NET_ADMIN"}}
	req.Secrets, req.Services, req.Retry = []string{"TOKEN"}, []string{"model"}, RetryPolicy{Task: 2}
	for name, change := range map[string]func(*Request){
		"an unconfigured service":                           func(r *Request) { r.Services = []string{"other"} },
		"an unconfigured secret":                            func(r *Request) { r.Secrets = []string{"OTHER"} },
		"a repeated secret":                                 func(r *Request) { r.Secrets = []string{"TOKEN", "TOKEN"} },
		"a secret that is also env":                         func(r *Request) { r.Env = map[string]string{"TOKEN": "x"} },
		"a launch command beyond the remote argument limit": func(r *Request) { r.Args = []string{strings.Repeat("x", ssh.MaxCommand)} },
	} {
		bad := req
		change(&bad)
		if _, err := s.Submit(ctx, bad); err == nil {
			t.Fatal("accepted", name)
		}
	}
	submit(t, s, req)
	listed, _ := json.Marshal(mustJob(t, s, "agent"))
	if strings.Contains(string(listed), "hunter2") || !slices.Equal(mustJob(t, s, "agent").Spec.Secrets, []string{"TOKEN"}) {
		t.Fatalf("jobs must name secrets without values: %s", listed)
	}

	h := newFakeHost(s.id)
	mustTick(t, s, workers[0], h)
	launched := h.launches[0]
	if !maps.Equal(launched.Secrets, config.Secrets) || !slices.Equal(launched.Spec.Services, []string{"model"}) || launched.Spec.Network != spec.Egress {
		t.Fatalf("launch lost the sandbox or configured secret: %+v", launched)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for name, dropped := range map[string]Config{"service": {Root: config.Root, Workers: workers, Secrets: config.Secrets}, "secret": {Root: config.Root, Workers: workers, Services: config.Services}} {
		if other, err := Open(dropped); err == nil {
			_ = other.Close()
			t.Fatalf("dropped a %s an unfinished job uses", name)
		}
	}
	if s, err = Open(config); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	stale := mustJob(t, s, "agent")
	if err := s.Stop("agent", "budget spent"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(stale); err != nil {
		t.Fatal(err)
	}
	if !mustJob(t, s, "agent").StopRequested {
		t.Fatal("a stale copy dropped the stop")
	}
	mustTick(t, s, workers[0], h)
	if !slices.Equal(h.stops, []string{launched.Attempt}) {
		t.Fatalf("stop not delivered: %q", h.stops)
	}
	mustTick(t, s, workers[0], h)
	j := mustJob(t, s, "agent")
	if at := j.Attempts[0]; j.State != Failed || j.StopReason != "budget spent" || at.StopRequested != Stop || at.Result == nil || at.Result.ExitCode != 137 {
		t.Fatalf("stopped job retried or lost its reason: %+v", j)
	}
	queued := req
	queued.ID = "queued"
	submit(t, s, queued)
	if err := s.Stop("queued", ""); err != nil {
		t.Fatal(err)
	}
	if j := mustJob(t, s, "queued"); j.State != Cancelled {
		t.Fatalf("stopping an unstarted job must cancel it: %s", j.State)
	}
}

func TestTimeoutsCountAgainstTheTask(t *testing.T) {
	f := newFixture(t)
	f.h.skew = -time.Hour
	for _, id := range []string{"stopped", "backstop"} {
		req := request(id)
		req.TimeoutSeconds = 60
		submit(t, f.s, req)
	}
	f.tick(t)
	f.tick(t)
	if len(f.h.stops) != 0 {
		t.Fatalf("a worker clock behind the server's stopped tasks early: %q", f.h.stops)
	}
	for _, c := range f.h.containers {
		c.StartedAt.Time = c.StartedAt.Add(-61 * time.Second)
	}
	backstop := mustJob(t, f.s, "backstop").Attempts[0].ID
	f.h.exit(backstop, 137)
	f.h.containers[backstop].finished = f.h.now()
	f.tick(t)
	stopped := mustJob(t, f.s, "stopped").Attempts[0]
	if !slices.Equal(f.h.stops, []string{stopped.ID}) || stopped.StopRequested != Timeout {
		t.Fatalf("expired task not stopped: %q %+v", f.h.stops, stopped)
	}
	f.tick(t)
	for _, id := range []string{"stopped", "backstop"} {
		j := mustJob(t, f.s, id)
		if at := j.Attempts[0]; j.State != Failed || at.StopRequested != Timeout || at.Failure != TaskFailure {
			t.Fatalf("%s: timeout not charged to the task: %+v", id, j)
		}
	}
}

func TestRebootDisappearanceForeignContainersAndSweep(t *testing.T) {
	f := newFixture(t)
	f.h.add("foreign", "other", podman.Running)
	submit(t, f.s, request("job"))
	f.tick(t)
	if info := f.s.Workers()[0].Info; len(f.h.launches) != 0 || info == nil || !strings.Contains(info.Error, "another server") {
		t.Fatalf("dispatched beside another server's containers: %+v", info)
	}
	delete(f.h.containers, "foreign")

	f.tick(t)
	first := mustJob(t, f.s, "job").Attempts[0]
	f.h.boot = "boot-2"
	f.tick(t)
	j := mustJob(t, f.s, "job")
	if j.State != Queued || j.Attempts[0].Failure != InfrastructureFailure || !slices.Contains(f.h.removed, first.ID) {
		t.Fatalf("reboot must fail the attempt as infrastructure: %+v", j)
	}

	f.tick(t)
	delete(f.h.containers, mustJob(t, f.s, "job").Attempts[1].ID)
	f.tick(t)
	if j = mustJob(t, f.s, "job"); j.State != Blocked {
		t.Fatalf("lost container must block the job: %+v", j)
	}
	if err := f.s.Cancel("job", false, ""); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j = mustJob(t, f.s, "job"); j.State != Cancelled {
		t.Fatalf("cancel must release a blocked job: %+v", j)
	}

	f.h.add(first.ID, f.s.id, podman.Exited)
	f.tick(t)
	if len(f.h.containers) != 0 {
		t.Fatalf("finished attempt's container not swept: %v", f.h.containers)
	}
}

func TestFinishedJobsLeaveMemoryAndListAsSummaries(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	cfg := testWorker("worker")
	s := open(t, state, []worker.Config{cfg}, nil)
	h := newFakeHost(s.id)
	submit(t, s, request("done"))
	submit(t, s, request("cancelled"))
	stale := mustJob(t, s, "cancelled")
	if err := s.Cancel("cancelled", false, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.save(stale); err != nil {
		t.Fatal(err)
	}
	if mustJob(t, s, "cancelled").State != Cancelled {
		t.Fatal("a stale copy revived a cancelled job")
	}
	mustTick(t, s, cfg, h)
	done := mustJob(t, s, "done")
	h.exit(done.current().ID, 0)
	mustTick(t, s, cfg, h)
	if len(s.jobs) != 0 {
		t.Fatalf("finished jobs stayed in memory: %v", slices.Collect(maps.Keys(s.jobs)))
	}
	if done = mustJob(t, s, "done"); done.State != Succeeded || done.current().Result == nil {
		t.Fatalf("finished job did not load from the store: %+v", done)
	}
	succeeded := s.List(Filter{States: []State{Succeeded}})
	if len(succeeded) != 1 || succeeded[0].ID != "done" || succeeded[0].Worker != "worker" || succeeded[0].Attempts != 1 {
		t.Fatalf("state filter: %+v", succeeded)
	}
	if listed := s.List(Filter{IDs: []string{"cancelled", "missing"}}); len(listed) != 1 || listed[0].State != Cancelled {
		t.Fatalf("ID filter: %+v", listed)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/jobs?state=succeeded&state=cancelled", nil))
	var listed []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 2 || listed[0]["spec"] != nil {
		t.Fatalf("GET /v1/jobs must list summaries: %s", w.Body.String())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, state, []worker.Config{cfg}, nil)
	defer func() { _ = s.Close() }()
	if len(s.jobs) != 0 || len(s.List(Filter{})) != 2 || mustJob(t, s, "done").State != Succeeded {
		t.Fatal("a restart lost finished jobs or kept them in memory")
	}
}

func TestFailedUploadRequeuesWithoutAnAttempt(t *testing.T) {
	f := newFixture(t)
	submit(t, f.s, request("job"))
	f.h.upload = errors.New("disk full")
	f.tick(t)
	if j := mustJob(t, f.s, "job"); j.State != Queued || len(j.Attempts) != 0 || j.Error != "disk full" {
		t.Fatalf("failed upload must requeue without an attempt: %+v", j)
	}
	f.tick(t)
	if j := mustJob(t, f.s, "job"); j.State != Running || len(j.Attempts) != 1 {
		t.Fatalf("requeued job did not launch: %+v", j)
	}
}
