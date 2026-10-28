package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
	return imagetest.New(testImageID)
}

func testTools() image.Tools { return image.Tools{Exec: testNix().Exec} }

type fakeContainer struct {
	podman.Container
	exit     int
	finished time.Time
}

// fakeHost is a worker with four CPUs whose containers run only when a test says so.
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
	seeds      []seeded
	runs       map[string]bool
	// lose is returned by the next launch after it creates the container; refuse, before.
	lose, refuse error
	broken       error
	// upload fails the next EnsureImage.
	upload error
}

func newFakeHost(server string) *fakeHost {
	return &fakeHost{server: server, boot: "boot-1", machine: "machine-1", containers: map[string]*fakeContainer{}, runs: map[string]bool{}}
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
	return worker.Probe{BootID: f.boot, Hardware: pool.Hardware{Hostname: "worker", MachineID: f.machine, Arch: "amd64", CPUs: []int{0, 1, 2, 3}, MemoryMiB: 1024}}, nil
}

func (f *fakeHost) Poll(context.Context) (worker.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := worker.Snapshot{BootID: f.boot, Now: f.now(), Runs: slices.Sorted(maps.Keys(f.runs))}
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

func (f *fakeHost) Seed(_ context.Context, attempt, prev, archive string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := os.Stat(archive); err != nil {
		return err
	}
	f.seeds = append(f.seeds, seeded{attempt, prev, f.runs[prev]})
	delete(f.runs, prev)
	return nil
}

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
	f.runs[l.Attempt] = true
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
	delete(f.runs, attempt)
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

func jobRequest(out string) Request {
	return Request{Out: out, Step: Step{Image: image.Source("/src#image"), Task: spec.Task{Resources: spec.Resources{Cores: 1, MemoryMiB: 128}}}}
}

func submit(t *testing.T, s *Server, r Request) string {
	t.Helper()
	j, err := s.Submit(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	return j.ID
}

// tick waits for the pass's background steps, as the next pass would find them still busy.
func tick(s *Server, cfg worker.Config, h host) error {
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

func mustTick(t *testing.T, s *Server, cfg worker.Config, h host) {
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

// ready ends a queued job's retry wait and the head start the other workers had.
func ready(t *testing.T, s *Server, id string) {
	t.Helper()
	j := mustJob(t, s, id)
	j.ReadyAt = time.Now().Add(-max(stepAffinity, avoidance))
	seed(t, s, j)
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
	dir string
}

func newFixture(t *testing.T) fixture {
	cfg := testWorker("worker")
	s := open(t, filepath.Join(t.TempDir(), "state"), []worker.Config{cfg}, nil)
	t.Cleanup(func() { _ = s.Close() })
	return fixture{s: s, cfg: cfg, h: newFakeHost(s.id), dir: t.TempDir()}
}

func (f fixture) tick(t *testing.T) {
	t.Helper()
	mustTick(t, f.s, f.cfg, f.h)
}

func (f fixture) out(name string) string { return filepath.Join(f.dir, name) }

func (f fixture) submit(t *testing.T, r Request) string {
	t.Helper()
	return submit(t, f.s, r)
}

func (f fixture) job(t *testing.T, id string) Job {
	t.Helper()
	return mustJob(t, f.s, id)
}

func readOut(t *testing.T, out, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(out, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestLostLaunchResponseAndRestartDoNotDuplicate(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	out := filepath.Join(t.TempDir(), "results", "one")
	workers := []worker.Config{testWorker("worker")}
	s := open(t, state, workers, nil)
	id := submit(t, s, jobRequest(out))
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
	if j := mustJob(t, s, id); len(j.Attempts) != 1 || j.State != Dispatching {
		t.Fatalf("dispatch not durable: %+v", j)
	}
	mustTick(t, s, workers[0], h)
	j := mustJob(t, s, id)
	at := j.Attempts[0]
	if len(h.launches) != 1 || j.State != Running || at.ContainerID != h.containers[at.ID].ID {
		t.Fatalf("restart did not adopt the launched container: %d launches, %+v", len(h.launches), j)
	}

	h.exit(at.ID, 0)
	mustTick(t, s, workers[0], h)
	j = mustJob(t, s, id)
	at = j.Attempts[0]
	if j.State != Delivering || at.Result == nil || at.Result.ExitCode != 0 || len(at.Result.OutputsSHA256) != 64 || len(h.containers) != 0 {
		t.Fatalf("collection: %+v, containers %v", j, h.containers)
	}
	record, err := readRecord(at.ResultDir)
	if err != nil || record.Attempt != at.ID || record.Out != out || record.Spec.Image != j.spec().Image || record.OutputsSHA256 != at.Result.OutputsSHA256 {
		t.Fatalf("run.json %+v: %v", record, err)
	}
	mustTick(t, s, workers[0], h)
	if j = mustJob(t, s, id); j.State != Succeeded {
		t.Fatalf("delivery: %+v", j)
	}
	if readOut(t, out, "score") != at.ID || readOut(t, out, ".ordo/1.stdout") != "out" || readOut(t, out, ".ordo/1.stderr") != "err" {
		t.Fatal("out lacks the outputs or logs")
	}
	var published Job
	if err := json.Unmarshal([]byte(readOut(t, out, ".ordo/job.json")), &published); err != nil || published.ID != id || published.State != Succeeded {
		t.Fatalf("job.json %+v: %v", published, err)
	}
	if _, err := os.Stat(filepath.Join(at.ResultDir, OutputsArchive)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("delivered outputs stayed on the server: %v", err)
	}
}

func TestRefusedLaunchRetriesAndChangedWorkerBlocks(t *testing.T) {
	f := newFixture(t)
	id := f.submit(t, jobRequest(f.out("job")))
	f.h.refuse = &ssh.ExitError{Code: 125, Stderr: "image not known"}
	f.tick(t)
	j := f.job(t, id)
	if j.State != Queued || j.Attempts[0].Failure != InfrastructureFailure || !slices.Contains(f.h.removed, j.Attempts[0].ID) {
		t.Fatalf("refused launch must requeue as an infrastructure failure: %+v", j)
	}
	ready(t, f.s, id)
	f.tick(t)
	if j = f.job(t, id); j.State != Running || len(j.Attempts) != 2 {
		t.Fatalf("retry not launched: %+v", j)
	}

	f.h.machine = "machine-2"
	f.tick(t)
	f.h.machine = "machine-1"
	f.tick(t)
	if j = f.job(t, id); j.State != Blocked || len(f.h.launches) != 1 {
		t.Fatalf("a changed worker must block the job until it is cancelled: %+v", j)
	}
	if err := f.s.Cancel(id, ""); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j = f.job(t, id); j.State != Cancelled || len(f.h.containers) != 0 {
		t.Fatalf("cancel must release a blocked job and sweep its container: %+v, %v", j, f.h.containers)
	}
	if _, err := os.Stat(f.out("job")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a cancelled job delivered results")
	}
}

func TestRetryReleasesTheFinishedAttempt(t *testing.T) {
	f := newFixture(t)
	req := jobRequest(f.out("whole"))
	req.Resources.Cores, req.Retry.Task = 4, 1
	id := f.submit(t, req)
	f.tick(t)
	first := f.job(t, id).Attempts[0]
	f.h.exit(first.ID, 1)
	f.tick(t)
	if j := f.job(t, id); j.State != Queued || j.Attempts[0].Failure != TaskFailure {
		t.Fatalf("failed task not requeued: %+v", j)
	}
	ready(t, f.s, id)
	f.tick(t)
	if j := f.job(t, id); j.State != Running || len(j.Attempts) != 2 {
		t.Fatalf("the finished attempt still holds the worker: %+v", j)
	}
}

func TestFailedCollectionBlocksOnce(t *testing.T) {
	f := newFixture(t)
	id := f.submit(t, jobRequest(f.out("job")))
	f.tick(t)
	at := f.job(t, id).Attempts[0]
	f.h.exit(at.ID, 0)
	f.h.broken = errors.New("archive links and special files are unsupported")
	f.tick(t)
	f.tick(t)
	if j := f.job(t, id); j.State != Blocked || f.h.collects != 1 || !strings.Contains(j.Error, "special files") {
		t.Fatalf("collection retried or not blocked: %d collects, %+v", f.h.collects, j)
	}
	if err := f.s.Cancel(id, "bad outputs"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j := f.job(t, id); j.State != Cancelled || len(f.h.containers) != 0 {
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
	req := jobRequest(filepath.Join(root, "tagged"))
	req.Tags = []string{"l40s", "fast"}
	if _, err := s.Submit(context.Background(), req); err == nil {
		t.Fatal("accepted tags that no single worker has")
	}
	req.Tags = []string{"l40s"}
	req.Resources.Cores = 5
	if _, err := s.Submit(context.Background(), req); err == nil || !strings.Contains(err.Error(), "5 cores") {
		t.Fatal("accepted a job larger than any worker with its tags", err)
	}
	req.Resources.Cores = 1
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
	dir := t.TempDir()
	secrets := t.TempDir()
	if err := os.WriteFile(filepath.Join(secrets, "TOKEN"), []byte("hunter2\nline 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := Config{Root: filepath.Join(t.TempDir(), "state"), Workers: []worker.Config{testWorker("worker")}, Services: map[string]string{"model": "127.0.0.1:8317"}, SecretsDir: secrets, Tools: testTools()}
	workers := config.Workers
	s, err := Open(config)
	if err != nil {
		t.Fatal(err)
	}
	req := jobRequest(filepath.Join(dir, "agent"))
	req.Sandbox = spec.Sandbox{Env: map[string]string{"MODE": "eval"}, Network: spec.Egress, CapAdd: []string{"NET_ADMIN"}}
	req.Secrets, req.Services, req.Retry = []string{"TOKEN"}, []string{"model"}, RetryPolicy{Task: 2}
	req.Entrypoint, req.Workdir = []string{"netlock", "agent"}, "/work"
	for name, change := range map[string]func(*Request){
		"an unconfigured service":                           func(r *Request) { r.Services = []string{"other"} },
		"an unconfigured secret":                            func(r *Request) { r.Secrets = []string{"OTHER"} },
		"a repeated secret":                                 func(r *Request) { r.Secrets = []string{"TOKEN", "TOKEN"} },
		"a launch command beyond the remote argument limit": func(r *Request) { r.Args = []string{strings.Repeat("x", ssh.MaxCommand)} },
	} {
		bad := req
		change(&bad)
		if _, err := s.Submit(ctx, bad); err == nil {
			t.Fatal("accepted", name)
		}
	}
	id := submit(t, s, req)
	listed, _ := json.Marshal(mustJob(t, s, id))
	if strings.Contains(string(listed), "hunter2") || !slices.Equal(mustJob(t, s, id).spec().Secrets, []string{"TOKEN"}) {
		t.Fatalf("jobs must name secrets without values: %s", listed)
	}

	h := newFakeHost(s.id)
	mustTick(t, s, workers[0], h)
	launched := h.launches[0]
	if !maps.Equal(launched.Secrets, map[string]string{"TOKEN": "hunter2\nline 2\n"}) || !slices.Equal(launched.Spec.Services, []string{"model"}) || launched.Spec.Network != spec.Egress || !slices.Equal(launched.Spec.Entrypoint, req.Entrypoint) || launched.Spec.Workdir != "/work" {
		t.Fatalf("launch lost the sandbox or configured secret: %+v", launched)
	}

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for name, dropped := range map[string]Config{"service": {Root: config.Root, Workers: workers, SecretsDir: secrets}, "secret": {Root: config.Root, Workers: workers, Services: config.Services}} {
		if other, err := Open(dropped); err == nil {
			_ = other.Close()
			t.Fatalf("dropped a %s an unfinished job uses", name)
		}
	}
	if s, err = Open(config); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	stale := mustJob(t, s, id)
	if err := s.Stop(id, "budget spent"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(stale); err != nil {
		t.Fatal(err)
	}
	if !mustJob(t, s, id).StopRequested {
		t.Fatal("a stale copy dropped the stop")
	}
	mustTick(t, s, workers[0], h)
	if !slices.Equal(h.stops, []string{launched.Attempt}) {
		t.Fatalf("stop not delivered: %q", h.stops)
	}
	mustTick(t, s, workers[0], h)
	mustTick(t, s, workers[0], h)
	j := mustJob(t, s, id)
	if at := j.Attempts[0]; j.State != Failed || len(j.Attempts) != 1 || at.StopRequested != Stop || at.StopReason != "budget spent" || at.Result == nil || at.Result.ExitCode != 137 {
		t.Fatalf("stopped job retried or lost its reason: %+v", j)
	}
	if readOut(t, filepath.Join(dir, "agent"), "score") != launched.Attempt {
		t.Fatal("a stopped job's result was not delivered")
	}
	queued := req
	queued.Out = filepath.Join(dir, "queued")
	queuedID := submit(t, s, queued)
	if err := s.Stop(queuedID, ""); err != nil {
		t.Fatal(err)
	}
	if j := mustJob(t, s, queuedID); j.State != Cancelled {
		t.Fatalf("stopping an unstarted job must cancel it: %s", j.State)
	}
}

func TestTimeoutsCountAgainstTheTask(t *testing.T) {
	f := newFixture(t)
	f.h.skew = -time.Hour
	ids := map[string]string{}
	for _, name := range []string{"stopped", "backstop"} {
		req := jobRequest(f.out(name))
		req.TimeoutSeconds = 60
		ids[name] = f.submit(t, req)
	}
	f.tick(t)
	f.tick(t)
	if len(f.h.stops) != 0 {
		t.Fatalf("a worker clock behind the server's stopped tasks early: %q", f.h.stops)
	}
	for _, c := range f.h.containers {
		c.StartedAt.Time = c.StartedAt.Add(-61 * time.Second)
	}
	backstop := f.job(t, ids["backstop"]).Attempts[0].ID
	f.h.exit(backstop, 137)
	f.h.containers[backstop].finished = f.h.now()
	f.tick(t)
	stopped := f.job(t, ids["stopped"]).Attempts[0]
	if !slices.Equal(f.h.stops, []string{stopped.ID}) || stopped.StopRequested != Timeout {
		t.Fatalf("expired task not stopped: %q %+v", f.h.stops, stopped)
	}
	f.tick(t)
	f.tick(t)
	for name, id := range ids {
		j := f.job(t, id)
		if at := j.Attempts[0]; j.State != Failed || at.StopRequested != Timeout || at.Failure != TaskFailure {
			t.Fatalf("%s: timeout not charged to the task: %+v", name, j)
		}
	}
}

func TestRebootDisappearanceForeignContainersAndSweep(t *testing.T) {
	f := newFixture(t)
	f.h.add("foreign", "other", podman.Running)
	id := f.submit(t, jobRequest(f.out("job")))
	f.tick(t)
	if info := f.s.Workers()[0].Info; len(f.h.launches) != 0 || info == nil || !strings.Contains(info.Error, "another server") {
		t.Fatalf("dispatched beside another server's containers: %+v", info)
	}
	delete(f.h.containers, "foreign")

	f.tick(t)
	first := f.job(t, id).Attempts[0]
	f.h.boot = "boot-2"
	f.tick(t)
	j := f.job(t, id)
	if j.State != Queued || j.Attempts[0].Failure != InfrastructureFailure || !slices.Contains(f.h.removed, first.ID) {
		t.Fatalf("reboot must fail the attempt as infrastructure: %+v", j)
	}

	ready(t, f.s, id)
	f.tick(t)
	delete(f.h.containers, f.job(t, id).Attempts[1].ID)
	f.tick(t)
	if j = f.job(t, id); j.State != Blocked {
		t.Fatalf("lost container must block the job: %+v", j)
	}
	if err := f.s.Cancel(id, ""); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if j = f.job(t, id); j.State != Cancelled {
		t.Fatalf("cancel must release a blocked job: %+v", j)
	}

	f.h.add(first.ID, f.s.id, podman.Exited)
	f.tick(t)
	if len(f.h.containers) != 0 {
		t.Fatalf("finished attempt's container not swept: %v", f.h.containers)
	}
}

func TestExpireForgetsOldFinishedJobsNoOneWaitsFor(t *testing.T) {
	f := newFixture(t)
	old := time.Now().Add(-retention - time.Hour)
	jobs := []Job{
		{ID: "expired", Out: f.out("expired"), State: Succeeded, CreatedAt: old, FinishedAt: old},
		{ID: "upstream", Out: f.out("upstream"), State: Succeeded, CreatedAt: old, FinishedAt: old},
		{ID: "recent", Out: f.out("recent"), State: Failed, CreatedAt: old},
		{ID: "waiting", Out: f.out("waiting"), State: Queued, CreatedAt: old, After: []string{"upstream"}, Waiting: []string{"in"}},
	}
	seed(t, f.s, jobs...)
	results := filepath.Join(f.s.root, "results", "expired")
	if err := os.MkdirAll(results, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := f.s.expire(time.Now()); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, r := range f.s.List(Filter{}) {
		listed = append(listed, r.ID)
	}
	slices.Sort(listed)
	if !slices.Equal(listed, []string{"recent", "upstream", "waiting"}) {
		t.Fatalf("listed after expiry: %q", listed)
	}
	if _, ok, _ := f.s.job("expired"); ok {
		t.Fatal("an expired job is still readable")
	}
	if _, err := os.Stat(results); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an expired job's results outlived it", err)
	}
}

func TestFinishedJobsLeaveMemoryAndListAsSummaries(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	dir := t.TempDir()
	cfg := testWorker("worker")
	s := open(t, state, []worker.Config{cfg}, nil)
	h := newFakeHost(s.id)
	done := submit(t, s, jobRequest(filepath.Join(dir, "done")))
	cancelled := submit(t, s, jobRequest(filepath.Join(dir, "cancelled")))
	stale := mustJob(t, s, cancelled)
	if err := s.Cancel(cancelled, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.save(stale); err != nil {
		t.Fatal(err)
	}
	if mustJob(t, s, cancelled).State != Cancelled {
		t.Fatal("a stale copy revived a cancelled job")
	}
	mustTick(t, s, cfg, h)
	h.exit(last(mustJob(t, s, done)).ID, 0)
	mustTick(t, s, cfg, h)
	mustTick(t, s, cfg, h)
	if len(s.jobs) != 0 || len(s.outs) != 0 {
		t.Fatalf("finished jobs stayed in memory: %v", slices.Collect(maps.Keys(s.jobs)))
	}
	if j := mustJob(t, s, done); j.State != Succeeded || j.current().Result == nil {
		t.Fatalf("finished job did not load from the store: %+v", j)
	}
	succeeded := s.List(Filter{States: []State{Succeeded}})
	if len(succeeded) != 1 || succeeded[0].ID != done || succeeded[0].Out != filepath.Join(dir, "done") || succeeded[0].Worker != "worker" || succeeded[0].Attempts != 1 {
		t.Fatalf("state filter: %+v", succeeded)
	}
	if listed := s.List(Filter{IDs: []string{cancelled, "missing"}}); len(listed) != 1 || listed[0].State != Cancelled {
		t.Fatalf("ID filter: %+v", listed)
	}
	if listed := s.List(Filter{Under: []string{dir}}); len(listed) != 2 {
		t.Fatalf("under filter: %+v", listed)
	}
	if listed := s.List(Filter{Under: []string{filepath.Join(dir, "do")}}); len(listed) != 0 {
		t.Fatalf("under matched a sibling's prefix: %+v", listed)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/jobs?state=succeeded&state=cancelled", nil))
	var listed []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed) != 2 || listed[0]["steps"] != 1.0 {
		t.Fatalf("GET /v1/jobs must list summaries: %s", w.Body.String())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, state, []worker.Config{cfg}, nil)
	defer func() { _ = s.Close() }()
	if len(s.jobs) != 0 || len(s.List(Filter{})) != 2 || mustJob(t, s, done).State != Succeeded {
		t.Fatal("a restart lost finished jobs or kept them in memory")
	}
}

func TestFailedUploadRequeuesWithoutAnAttempt(t *testing.T) {
	f := newFixture(t)
	id := f.submit(t, jobRequest(f.out("job")))
	f.h.upload = errors.New("disk full")
	f.tick(t)
	if j := f.job(t, id); j.State != Queued || len(j.Attempts) != 0 || j.Error != "disk full" {
		t.Fatalf("failed upload must requeue without an attempt: %+v", j)
	}
	pending := func() string {
		t.Helper()
		f.tick(t)
		listed := f.s.List(Filter{IDs: []string{id}})
		if len(listed) != 1 || listed[0].State != Queued {
			t.Fatalf("job left the queue before its retry: %+v", listed)
		}
		return listed[0].Pending["worker"]
	}
	if reason := pending(); !strings.Contains(reason, "retry waits") {
		t.Fatalf("a retry must wait out its backoff: %q", reason)
	}
	j := f.job(t, id)
	j.ReadyAt = time.Now().Add(-time.Second)
	seed(t, f.s, j)
	if reason := pending(); !strings.Contains(reason, "failed here") {
		t.Fatalf("the worker a retry failed on must give the others a head start: %q", reason)
	}
	ready(t, f.s, id)
	f.tick(t)
	if j := f.job(t, id); j.State != Running || len(j.Attempts) != 1 {
		t.Fatalf("requeued job did not launch: %+v", j)
	}
}

func TestDefaultWorkerStateDirCachesHome(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	w := testWorker("worker")
	w.StateDir = ""
	calls := 0
	c := Config{Root: root, Workers: []worker.Config{w}, Tools: testTools(), Home: func(_ context.Context, host string) (string, error) {
		calls++
		return "/home/" + host, nil
	}}
	for range 2 {
		s, err := Open(c)
		if err != nil {
			t.Fatal(err)
		}
		if dir := s.settings.Load().workers[0].StateDir; dir != "/home/worker/.local/state/ordo/worker" {
			t.Fatalf("state dir %q", dir)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		c.Home = func(context.Context, string) (string, error) { return "", errors.New("unreachable") }
	}
	if calls != 1 {
		t.Fatalf("resolved home %d times", calls)
	}
	c.Root = filepath.Join(t.TempDir(), "fresh")
	if _, err := Open(c); err == nil || !strings.Contains(err.Error(), "set state_dir") {
		t.Fatalf("unresolved home: %v", err)
	}
	c.Home = func(context.Context, string) (string, error) { return "relative", nil }
	if _, err := Open(c); err == nil {
		t.Fatal("accepted a relative home")
	}
}

func (f *fakeHost) Release(_ context.Context, attempt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.containers, attempt)
	return nil
}

// seeded is one Seed call; moved tells whether prev's outputs were still on the worker.
type seeded struct {
	attempt, prev string
	moved         bool
}

// Submit queues one request as a batch of one would.
func (s *Server) Submit(ctx context.Context, r Request) (Job, error) {
	result := slices.Collect(s.SubmitBatch(ctx, []BatchEntry{{Line: 1, Request: r}}))[0]
	switch {
	case result.Error != "":
		return Job{}, errors.New(result.Error)
	case result.Done:
		return Job{}, fmt.Errorf("%s already holds results", r.Out)
	}
	j, _, err := s.job(result.ID)
	return j, err
}

func TestSecretMissingAtDispatchAbandonsTheAttempt(t *testing.T) {
	secrets := t.TempDir()
	file := filepath.Join(secrets, "TOKEN")
	if err := os.WriteFile(file, []byte("hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(Config{Root: filepath.Join(t.TempDir(), "state"), Workers: []worker.Config{testWorker("worker")}, SecretsDir: secrets, Tools: testTools()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	req := jobRequest(filepath.Join(t.TempDir(), "agent"))
	req.Secrets = []string{"TOKEN"}
	id := submit(t, s, req)
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	h := newFakeHost(s.id)
	mustTick(t, s, s.settings.Load().workers[0], h)
	if j := mustJob(t, s, id); len(h.launches) != 0 || !strings.Contains(j.Error, "secret TOKEN") {
		t.Fatalf("launched without its secret: launches=%d error=%q", len(h.launches), j.Error)
	}
}

func TestReloadRejectsStrandingUnfinishedJobs(t *testing.T) {
	root := t.TempDir()
	a, b := testWorker("a"), testWorker("b", "fast")
	s := open(t, filepath.Join(root, "state"), []worker.Config{a, b}, nil)
	defer func() { _ = s.Close() }()
	running := submit(t, s, jobRequest(filepath.Join(root, "running")))
	mustTick(t, s, a, newFakeHost(s.id))
	fast := jobRequest(filepath.Join(root, "fast"))
	fast.Tags = []string{"fast"}
	submit(t, s, fast)
	moved := a
	moved.StateDir = "/srv/moved"
	for want, workers := range map[string][]worker.Config{"removes worker a": {b}, "moves the state_dir": {moved, b}, "fast": {a, testWorker("b")}} {
		if err := s.Reload(Config{Workers: workers}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("reload to %+v: %v, want %q", workers, err, want)
		}
	}
	if j := mustJob(t, s, running); len(j.Attempts) != 1 || j.current().Worker != "a" {
		t.Fatalf("running job %+v", j)
	}
}

func TestReloadChecksOnlyStepsAhead(t *testing.T) {
	root := t.TempDir()
	s := open(t, filepath.Join(root, "state"), []worker.Config{testWorker("a", "fast")}, nil)
	defer func() { _ = s.Close() }()
	stage := func(tags ...string) Stage {
		return Stage{Spec: spec.Spec{Task: spec.Task{Tags: tags, Resources: spec.Resources{Cores: 1, MemoryMiB: 128}}}}
	}
	seed(t, s,
		Job{ID: newID(), Out: filepath.Join(root, "second"), State: Queued, Step: 1, Steps: []Stage{stage("fast"), stage()}},
		Job{ID: newID(), Out: filepath.Join(root, "delivering"), State: Delivering, Steps: []Stage{stage("fast")}},
	)
	if err := s.Reload(Config{Workers: []worker.Config{testWorker("a")}}); err != nil {
		t.Fatal("a tag only finished steps used blocked the reload:", err)
	}
}

func TestReloadRestartsOnlyChangedWorkers(t *testing.T) {
	a, b := testWorker("a"), testWorker("b")
	s := open(t, filepath.Join(t.TempDir(), "state"), []worker.Config{a, b}, nil)
	defer func() { _ = s.Close() }()
	sessions := map[string]context.Context{}
	for _, host := range []string{"a", "b"} {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		sessions[host], s.sessions[host] = ctx, cancel
	}
	b.CPUs = "0-7"
	if err := s.Reload(Config{Workers: []worker.Config{a, b, testWorker("c")}}); err != nil {
		t.Fatal(err)
	}
	if context.Cause(sessions["a"]) != nil || !errors.Is(context.Cause(sessions["b"]), errReconfigured) {
		t.Fatalf("restarted a=%v b=%v, want only b", context.Cause(sessions["a"]), context.Cause(sessions["b"]))
	}
	if workers := s.Workers(); len(workers) != 3 || workers[2].Host != "c" {
		t.Fatalf("workers %+v", workers)
	}
}

func TestAdmitChecksReloadedWorkers(t *testing.T) {
	root := t.TempDir()
	s := open(t, filepath.Join(root, "state"), []worker.Config{testWorker("a", "gone")}, nil)
	defer func() { _ = s.Close() }()
	if err := s.Reload(Config{Workers: []worker.Config{testWorker("a")}}); err != nil {
		t.Fatal(err)
	}
	late := Job{ID: newID(), Out: filepath.Join(root, "late"), State: Queued, Steps: []Stage{{Spec: spec.Spec{Task: spec.Task{Tags: []string{"gone"}, Resources: spec.Resources{Cores: 1, MemoryMiB: 128}}}}}}
	result := &BatchResult{}
	s.admit([]Job{late}, []*BatchResult{result})
	if !strings.Contains(result.Error, "tags") {
		t.Fatalf("admitted a job the reloaded workers cannot run: %+v", result)
	}
}

func TestRetiredSessionDoesNotDispatch(t *testing.T) {
	cfg := testWorker("worker")
	s := open(t, filepath.Join(t.TempDir(), "state"), []worker.Config{cfg}, nil)
	defer func() { _ = s.Close() }()
	id := submit(t, s, jobRequest(filepath.Join(t.TempDir(), "out")))
	h := newFakeHost(s.id)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errReconfigured)
	probe, err := h.Probe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.pass(ctx, cfg, h, &probe, &steps{cancel: cancel}); !errors.Is(err, errReconfigured) {
		t.Fatalf("pass ended with %v", err)
	}
	if j := mustJob(t, s, id); j.State != Queued || len(j.Attempts) != 0 {
		t.Fatalf("a retired session dispatched %+v", j)
	}
}
