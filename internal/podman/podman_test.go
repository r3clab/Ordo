package podman

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/spec"
)

func TestRunArgv(t *testing.T) {
	r := Run{Attempt: "a1", Server: "s1", Image: "sha256:" + strings.Repeat("a", 64), Args: []string{"argument with spaces", "$(not-a-shell)"}, CPUs: []int{0, 64}, MemoryMiB: 128, Outputs: "/w/runs/a1/outputs"}
	want := []string{
		"run", "--detach", "--pull=never", "--name", "ordo-a1",
		"--label", "io.r3clab.ordo.attempt=a1", "--label", "io.r3clab.ordo.server=s1",
		"--cpuset-cpus", "0,64", "--memory", "134217728", "--memory-swap", "134217728",
		"--network=none", "--image-volume=ignore", "--log-driver=k8s-file",
		"--volume", "/w/runs/a1/outputs:/outputs:rw",
		"--", r.Image, "argument with spaces", "$(not-a-shell)",
	}
	if got := r.Argv(); !slices.Equal(got, want) {
		t.Fatalf("argv = %q, want %q", got, want)
	}

	r.Sandbox = spec.Sandbox{Env: map[string]string{"B": "2", "A": "1"}, Network: spec.Egress, CapAdd: []string{"NET_ADMIN"}}
	r.Inputs = map[string]string{"data": "/w/trees/h"}
	r.Services = map[string]string{"model": "/w/services/model"}
	r.GPUs = []string{"GPU-11111111-1111-1111-1111-111111111111"}
	r.CDISpecDir = "/w/cdi"
	r.Secrets = []string{"TOKEN"}
	r.Timeout = 70
	r.Entrypoint = []string{"wrap", "--flag", `"quoted"`}
	r.Workdir = "/work"
	got := r.Argv()
	if !slices.Equal(got[:2], []string{"--cdi-spec-dir", "/w/cdi"}) {
		t.Fatalf("CDI directory must be a global flag: %q", got)
	}
	for _, want := range [][]string{
		{"--timeout", "70", "--network=pasta", "--cap-add=NET_ADMIN", "--env", "A=1", "--env", "B=2", "--entrypoint", `["wrap","--flag","\"quoted\""]`, "--workdir", "/work", "--image-volume=ignore"},
		{"--volume", "/w/trees/h:/inputs/data:ro", "--volume", "/w/services/model:/services/model:ro", "--volume", "/w/runs/a1/outputs:/outputs:rw"},
		{"--device", "nvidia.com/gpu=GPU-11111111-1111-1111-1111-111111111111", "--env", "NVIDIA_VISIBLE_DEVICES=void", "--secret", "ordo-a1-TOKEN,type=mount,target=/run/secrets/TOKEN", "--"},
	} {
		start := slices.Index(got, want[0])
		if start < 0 || !slices.Equal(got[start:start+len(want)], want) {
			t.Fatalf("argv %q lacks %q", got, want)
		}
	}
}

func TestParsePSAcceptsBothTimeFormats(t *testing.T) {
	data := `[{"Id":"x","State":"running","Labels":{"io.r3clab.ordo.attempt":"a1","io.r3clab.ordo.server":"s1"},"Created":1700000000,"StartedAt":1700000005},
	{"Id":"y","State":"created","Labels":null,"Created":"2026-01-01T00:00:00Z","StartedAt":0}]`
	containers, err := ParsePS([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if c := containers[0]; c.Attempt() != "a1" || c.Server() != "s1" || !c.StartedAt.Equal(time.Unix(1700000005, 0)) {
		t.Fatalf("first container %+v", c)
	}
	if c := containers[1]; !c.StartedAt.IsZero() || c.Created.Year() != 2026 || c.Attempt() != "" {
		t.Fatalf("second container %+v", c)
	}
}

func TestCheckInfo(t *testing.T) {
	good := `{"host":{"os":"linux","cgroupVersion":"v2","cgroupControllers":["cpuset","memory","pids"],"security":{"rootless":true}}}`
	if err := CheckInfo([]byte(good)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		`{"host":{"os":"linux","cgroupVersion":"v2","cgroupControllers":["cpuset","memory"],"security":{"rootless":false}}}`,
		`{"host":{"os":"linux","cgroupVersion":"v1","cgroupControllers":["cpuset","memory"],"security":{"rootless":true}}}`,
		`{"host":{"os":"linux","cgroupVersion":"v2","cgroupControllers":["memory"],"security":{"rootless":true}}}`,
	} {
		if err := CheckInfo([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}
