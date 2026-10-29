package worker

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/r3clab/Ordo/internal/podman"
	"github.com/r3clab/Ordo/internal/pool"
	"github.com/r3clab/Ordo/internal/spec"
	"github.com/r3clab/Ordo/internal/ssh"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__ssh" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		if err := ssh.Supervise(ctx, os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

// This opt-in test starts an isolated, unprivileged sshd; it never changes the host's SSH config.
func TestSSHMaxSessions(t *testing.T) {
	if os.Getenv("_ORDO_TEST_CONTROLLER") != "" {
		var init initialization
		if err := json.Unmarshal([]byte(os.Getenv("_ORDO_TEST_INIT")), &init); err != nil {
			t.Fatal(err)
		}
		m, err := ssh.Dial(context.Background(), "worker", os.Getenv("_ORDO_TEST_CONFIG"), init, func(string) (string, error) { return os.Getenv("_ORDO_TEST_BINARY"), nil })
		if err != nil {
			t.Fatal(err)
		}
		for name, address := range init.Services {
			forward, err := init.Config.ServiceForward(name, address)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.Forward(context.Background(), forward); err != nil {
				t.Fatal(err)
			}
		}
		fmt.Println(m.Socket)
		select {}
	}
	if os.Getenv("ORDO_TEST_SSHD") != "1" {
		t.Skip("set ORDO_TEST_SSHD=1 to test an isolated local sshd with MaxSessions 1")
	}
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "ordo-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	image := os.Getenv("ORDO_TEST_PODMAN_IMAGE")
	log := ""
	if image == "" {
		log = fakes(t)
	} else if !spec.ValidImageID(image) {
		t.Fatal("ORDO_TEST_PODMAN_IMAGE must be a full sha256 image ID already present locally")
	}
	run := func(name string, args ...string) {
		t.Helper()
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
	}
	for _, key := range []string{"host", "client"} {
		run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(dir, key))
	}
	binary := filepath.Join(dir, "ordo")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/ordo")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	// User shell startup files may replace sshd's PATH; isolate the fixture's tools and cache.
	wrapper := "#!/bin/sh\nexport PATH=" + ssh.Quote(os.Getenv("PATH")) + " XDG_CACHE_HOME=" + ssh.Quote(filepath.Join(dir, "cache")) + " TMPDIR=" + ssh.Quote(dir) + " ORDO_FAKE_LOG=" + ssh.Quote(log) + " XDG_RUNTIME_DIR=" + ssh.Quote(os.Getenv("XDG_RUNTIME_DIR")) + " DBUS_SESSION_BUS_ADDRESS=" + ssh.Quote(os.Getenv("DBUS_SESSION_BUS_ADDRESS")) + "\nexec /bin/sh -c \"$SSH_ORIGINAL_COMMAND\"\n"
	if err := os.WriteFile(filepath.Join(dir, "shell"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "sshd_config")
	text := fmt.Sprintf(`Port %d
ListenAddress 127.0.0.1
HostKey %s/host
PidFile %s/sshd.pid
AuthorizedKeysFile %s/client.pub
ForceCommand %s/shell
StrictModes no
UsePAM no
PasswordAuthentication no
KbdInteractiveAuthentication no
AuthenticationMethods publickey
AllowUsers %s
MaxSessions 1
AllowStreamLocalForwarding yes
LogLevel VERBOSE
SetEnv "XDG_CACHE_HOME=%s/cache" "TMPDIR=%s" "PATH=%s" "ORDO_FAKE_LOG=%s"
`, port, dir, dir, dir, dir, me.Username, dir, dir, os.Getenv("PATH"), log)
	if err := os.WriteFile(config, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	sshdLog, err := os.Create(filepath.Join(dir, "sshd.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sshdLog.Close() }()
	daemon := exec.Command(sshd, "-D", "-e", "-f", config)
	daemon.Stdout, daemon.Stderr = sshdLog, sshdLog
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = daemon.Process.Kill()
		_ = daemon.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(sshdLog.Name())
			t.Logf("sshd:\n%s", data)
		}
	})
	hostKey, err := os.ReadFile(filepath.Join(dir, "host.pub"))
	if err != nil {
		t.Fatal(err)
	}
	known := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(known, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", port, hostKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	sshConfig := filepath.Join(dir, "ssh_config")
	clientConfig := fmt.Sprintf("Host worker\n HostName 127.0.0.1\n Port %d\n User %s\n IdentityFile %s/client\n IdentitiesOnly yes\n StrictHostKeyChecking yes\n UserKnownHostsFile %s\n", port, me.Username, dir, known)
	if err := os.WriteFile(sshConfig, []byte(clientConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sshd did not start", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	h := apiHost(t)
	if image != "" {
		online, err := os.ReadFile("/sys/devices/system/cpu/online")
		if err != nil {
			t.Fatal(err)
		}
		cpus, err := spec.ParseCPUs(strings.TrimSpace(string(online)))
		if err != nil || len(cpus) == 0 {
			t.Fatal("cannot select a CPU", err)
		}
		h.CPUs = strconv.Itoa(cpus[0])
	}
	init := initialization{Protocol: protocol, Config: h.Config, Server: "s1"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m, err := ssh.Dial(ctx, "worker", sshConfig, init, func(string) (string, error) { return binary, nil })
	if err != nil {
		t.Fatal(err)
	}
	c := socketClient(m.Socket)
	c.master = m
	t.Cleanup(func() { _ = c.Close() })
	// Verify that a second exec channel really is forbidden, including OpenSSH's direct fallback.
	probe := exec.CommandContext(ctx, "ssh", "-F", sshConfig, "-S", filepath.Join(filepath.Dir(m.Socket), "master"), "-oProxyCommand=false", "worker", "true")
	if out, err := probe.CombinedOutput(); err == nil {
		t.Fatalf("sshd unexpectedly accepted a second session: %s", out)
	}
	var uploads sync.WaitGroup
	for range 2 {
		in, writer := io.Pipe()
		uploads.Go(func() { _ = c.stream(ctx, http.MethodPut, "inputs/"+strings.Repeat("a", 64), in, nil); _ = in.Close() })
		// A partial tar holds a transfer open while control requests run.
		go func() { _, _ = writer.Write(make([]byte, 4096)); <-ctx.Done(); _ = writer.CloseWithError(ctx.Err()) }()
	}
	for range 8 {
		if _, err := c.Poll(ctx); err != nil {
			t.Fatal("concurrent API request consumed an SSH session", err)
		}
	}
	cancel()
	uploads.Wait()
	_ = c.Close()
	// Graceful reconnection uses the cached binary and the same existing worker state.
	m, err = ssh.Dial(context.Background(), "worker", sshConfig, init, func(string) (string, error) { return binary, nil })
	if err != nil {
		t.Fatal(err)
	}
	c = socketClient(m.Socket)
	c.master = m
	if _, err := c.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	// An active service forward must not keep ssh alive after the controller is SIGKILLed.
	service, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	init.Services = map[string]string{"test": service.Addr().String()}
	data, _ := json.Marshal(init)
	controller := exec.Command(os.Args[0], "-test.run=^TestSSHMaxSessions$")
	controller.Env = append(os.Environ(), "_ORDO_TEST_CONTROLLER=1", "_ORDO_TEST_INIT="+string(data), "_ORDO_TEST_CONFIG="+sshConfig, "_ORDO_TEST_BINARY="+binary)
	var stderr bytes.Buffer
	controller.Stderr = &stderr
	out, err := controller.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = controller.Process.Kill(); _ = controller.Wait() })
	scan := bufio.NewScanner(out)
	if !scan.Scan() {
		t.Fatalf("controller failed: %s", stderr.String())
	}
	socket := scan.Text()
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(socket)) })
	api := socketClient(socket)
	defer func() { _ = api.Close() }()
	if _, err := api.Poll(context.Background()); err != nil {
		t.Fatalf("controller API %q: %v", socket, err)
	}
	attempt, container := "", ""
	if image != "" {
		if _, err := api.Probe(context.Background()); err != nil {
			t.Fatal(err)
		}
		attempt = strings.ToLower(rand.Text())
		t.Cleanup(func() {
			if out, err := exec.Command("podman", "rm", "--force", "--ignore", podman.Name(attempt)).CombinedOutput(); err != nil {
				t.Errorf("container cleanup: %v: %s", err, out)
			}
		})
		container, err = api.Launch(context.Background(), Launch{Attempt: attempt,
			Spec:       spec.Spec{Image: image, Task: spec.Task{Command: spec.Command{Args: []string{"-c", "echo once >> /outputs/count; sleep 60"}}, Resources: spec.Resources{Cores: 1, MemoryMiB: 128}, TimeoutSeconds: 90}},
			Allocation: pool.Allocation{CPUs: h.cpus(), MemoryMiB: 128}})
		if err != nil {
			t.Fatal(err)
		}
	}
	forwarded, err := net.Dial("unix", h.serviceSocket("test"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = forwarded.Close() }()
	_ = service.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	accepted, err := service.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accepted.Close() }()
	children, err := exec.Command("ps", "-o", "pid=", "--ppid", strconv.Itoa(controller.Process.Pid)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(string(children))) != 1 {
		t.Fatalf("unexpected controller children: %s", children)
	}
	supervisorPID, err := strconv.Atoi(strings.TrimSpace(string(children)))
	if err != nil {
		t.Fatal(err)
	}
	children, err = exec.Command("ps", "-o", "pid=", "--ppid", strconv.Itoa(supervisorPID)).Output()
	if err != nil {
		t.Fatal(err)
	}
	sshPID, err := strconv.Atoi(strings.TrimSpace(string(children)))
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = controller.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for {
		_, err := os.Stat(fmt.Sprintf("/proc/%d", sshPID))
		if os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("SSH process %d survived controller SIGKILL with an active forward", sshPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if dirs, _ := filepath.Glob(filepath.Join(dir, "ordo-worker-*")); len(dirs) != 0 {
		t.Fatalf("helper socket directories survived: %v", dirs)
	}
	if image != "" {
		m, err := ssh.Dial(context.Background(), "worker", sshConfig, init, func(string) (string, error) { return binary, nil })
		if err != nil {
			t.Fatal(err)
		}
		reconnected := socketClient(m.Socket)
		reconnected.master = m
		defer func() { _ = reconnected.Close() }()
		found, err := reconnected.Inspect(context.Background(), attempt)
		if err != nil || found.ID != container || found.State.Status != podman.Running {
			t.Fatalf("container did not survive controller death: %+v %v", found, err)
		}
		if err := reconnected.Stop(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(15 * time.Second)
		for {
			found, err = reconnected.Inspect(context.Background(), attempt)
			if err != nil {
				t.Fatal(err)
			}
			if found.State.Status != podman.Running {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("container did not stop")
			}
			time.Sleep(100 * time.Millisecond)
		}
		results := t.TempDir()
		if err := reconnected.Collect(context.Background(), attempt, results); err != nil {
			t.Fatal(err)
		}
		if count, err := os.ReadFile(filepath.Join(results, OutputsDir, "count")); err != nil || string(count) != "once\n" {
			t.Fatalf("execution count: %q %v", count, err)
		}
		if err := reconnected.Remove(context.Background(), attempt); err != nil {
			t.Fatal(err)
		}
	}
}
