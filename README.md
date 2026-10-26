# Ordo

A task dispatcher and scheduler for fair, reproducible evaluations across SSH-accessible workers.
Every task runs in rootless Podman using a [nix2container](https://github.com/nlewo/nix2container)-built image.

**Status:** Local CPU-only execution/recovery and persistent SSH worker agents are implemented.
Agents provide durable, idempotent attempts, exclusive physical cores, and aggregate memory accounting.
Immutable image/input transfer and output collection are implemented.
The durable server queue reconnects and reconciles original attempts after controller failure.
JSON DAGs, parameter sweeps, bounded retries, timeouts, and cancellation are implemented.
GPU allocation and web UI are still planned.

## Direction

Build a focused, single-user evaluation runner for an initial pool of **1–10 workers**, not a general-purpose orchestration platform.
Use **Go** for the server, worker agents, and CLI.

Learn from and selectively adapt:

- [Nomad](https://github.com/hashicorp/nomad) and its [Podman driver](https://github.com/hashicorp/nomad-driver-podman): task lifecycle, container recovery, and resource enforcement.
- [HyperQueue](https://github.com/It4innovations/hyperqueue): dependency-aware scheduling, resource accounting, and indexed CPU/GPU allocation.

Reuse small, relevant code paths and their tests; port HyperQueue logic where worthwhile.
Neither upstream runtime is required.
Prefer a small Go implementation over importing Nomad's full control plane or maintaining a Go/Rust split.

## Deployment

```text
CLI / workflow files / web UI → Ordo server
                                   ↕ SSH tunnels
                              Worker agents → rootless Podman tasks
```

- **Server:** has Nix and Podman installed; owns the queue, scheduling decisions, and durable run records.
- **Workers:** each has Nix and Podman installed.
  SSH launches persistent worker agents; agent communication uses SSH tunnels.
- **Tasks:** declare a container image, command, inputs, resource requirements, and outputs.
  Every task runs in **rootless Podman**, never directly on the host.
- **Nix:** provides reproducible tooling and environment builds; Podman remains the task runtime.

Workers must support the resource controls and GPU permissions a task requires.
Unsupported allocations must be rejected, not silently weakened.

## Fair evaluation

Fairness means giving compared runs the same declared hardware resources, not fair-share scheduling between users.

- Assign exclusive CPU cores and GPU devices where requested, with consistent memory budgets.
  Concurrent runs must not share their allocated cores or GPUs.
- Let users define hardware-matching requirements per evaluation: relaxed CPU-model matching may be acceptable for some experiments; others require identical hardware configurations.
- Pin container images by digest and record code, input, configuration, and seed versions where applicable.
- Preserve worker details, resource assignments, timings, exit status, logs, and outputs.
  Keep failed and retried attempts visible.

Exclusive cores and GPUs do not isolate shared caches, memory bandwidth, or unrelated host activity; these remain measurement risks.

## Workflows and recovery

- Provide a CLI, declarative workflow files, and a web UI for your own trusted workloads.
- Support independent tasks, parameter sweeps, and dependency graphs.
- Manage input transfers and output collection over SSH, caching reusable inputs.
- Keep tasks running through server restarts or tunnel disconnects.
  Workers retain execution state and results locally, then reconcile with the server before retries or downstream scheduling.
- Support infrastructure-failure retries and user-defined task retry policies, with bounded attempts.
  A disconnect alone is not proof that a task failed.

## Run a local task

Load an image from your evaluation's Nix flake, then use the image reference printed by `copyToPodman`:

```sh
nix run /path/to/evaluation#image.copyToPodman
nix run . -- run --image evaluation:BUILD_TAG \
  --cpus 0,1 --memory-mib 512 --output ./run-001
```

Ordo resolves the local image reference to an immutable image ID and never pulls from a registry.
The image's ENTRYPOINT/CMD runs by default; trailing `-- COMMAND ARGS...` replaces CMD while preserving ENTRYPOINT, without invoking a host shell.

- Requires local Linux, rootless Podman, and cgroup v2 with delegated `cpuset` and `memory` controllers; unsupported hosts fail preflight.
- Pins logical CPU IDs and limits memory with swap disabled.
  Standalone `run` is **unmanaged**: pinning does not reserve CPUs and can overlap agent-managed tasks or other processes.
- The output directory must not exist.
  `run.json` records the request, image/container identity, timestamps, exit status, and infrastructure errors; `stdout.log` and `stderr.log` are collected after exit.
- Removes the container only after logs and final metadata have been saved.
  On interruption or collection failure, it is retained and **may still be running**; use `ordo recover --output DIR` to collect it, or `container_name` from `run.json` with `podman inspect`, `podman logs`, or `podman stop` for manual management.
- Only logs and metadata are exported; container filesystem changes are discarded on cleanup.
  Networking, file mounts/artifact export, GPUs, retries, and automatic reconnection are not included in this slice.

Task exit codes are propagated; CLI/infrastructure errors return 2 and interruption returns 130.
For an opt-in live test, use a preloaded nix2container image whose default command exits successfully:

```sh
ORDO_TEST_IMAGE=evaluation:BUILD_TAG ORDO_TEST_CPUS=0 \
  go test -v ./internal/task -run TestPodmanIntegration
```

## Recover a retained local task

On the same host, as the same user and using the same local Podman storage:

```sh
nix run . -- recover --output ./run-001
```

Recovery never creates, starts, restarts, or retries a task.
It verifies the saved container and image identity, waits if the container is still running, and uses the same collection and cleanup path as `run`.
If launch was interrupted before the container ID was saved, an Ordo container label proves ownership; older unlabeled containers without a saved ID require manual inspection.

- Logs are retrieved into temporary files and synced before publication; each destination is replaced atomically.
  Retrieval failures leave previous logs untouched.
  The container is retained until both logs and final result metadata are saved.
- A durable `finished` record marks saved results independently of cleanup errors.
  Recovery can retry cleanup or reconcile removal that succeeded before its metadata update.
- Repeating recovery after successful cleanup returns the saved task exit code without contacting Podman.
- Missing containers without committed results, inconsistent metadata, and unsupported container states cause errors, not automatic reruns.
  Finished records must still have their saved log files.
- An advisory lock serializes Ordo processes using the same local run directory; it is released when the process exits, including crashes.
  Do not delete `.lock` or modify the directory while Ordo is using it.
  Stop the original CLI before recovering a run from an older Ordo version that did not acquire this lock.

Recovery does not require resource controllers to remain delegated because it makes no new allocation.
It is explicit, local reconciliation—not an SSH worker, automatic reconnect, or retry policy.

## Managed workers

Workers require Linux, rootless Podman with delegated cgroup v2 controls, OpenSSH, a user systemd manager, and existing user lingering.
`worker start` uploads a checksum-verified, content-addressed static Linux executable and starts a transient user service; it does not enable a permanent service or change host configuration.
Use a worker-local filesystem for state, not a shared network filesystem.

```sh
# Replace the host, remote path, and logical CPU pool for your worker's topology.
ordo worker start --host WORKER --state /home/worker/.local/state/ordo \
  --cpus 0,1,64,65 --memory-mib 512 --binary ./ordo-linux-amd64

# Transfer a local nix2container image; use the immutable ID returned in JSON.
ordo worker put-image --host WORKER --state /home/worker/.local/state/ordo \
  --cpus 0,1,64,65 --memory-mib 512 --image localhost/evaluation:BUILD_TAG

# Alternatively supply --archive OCI_ARCHIVE --image sha256:IMAGE_ID.
# Named inputs mount read-only at /inputs/NAME; write artifacts under /outputs.
ordo worker submit --host WORKER --state /home/worker/.local/state/ordo \
  --cpus 0,1,64,65 --memory-mib 512 \
  --id evaluation-001 --image sha256:IMAGE_ID --cores 1 --task-memory-mib 256

ordo worker get --host WORKER --state /home/worker/.local/state/ordo \
  --cpus 0,1,64,65 --memory-mib 512 --id evaluation-001
```

- The CPU pool must contain every online SMT sibling of each selected physical core.
  Requests count **physical cores**, not logical CPUs.
- Agent-managed attempts cannot overlap physical cores; their memory limits must fit the configured aggregate budget.
  Standalone runs and unrelated host processes are outside this guarantee.
- An attempt ID identifies one immutable specification.
  Repeating the same submission returns that attempt; changing its specification is rejected.
- SQLite commits the attempt and its reservation before launch.
  Restart restores reservations before admission.
  Unknown outcomes remain blocked with resources held, never silently retried.
- The agent owns a private mode-0600 Unix socket, reached through a separately owned SSH tunnel.
  Closing the client/tunnel does not cancel work.
- Podman operations launched from a systemd service use independent transient runtime scopes.
  Container monitors and FUSE mounts survive agent service cleanup; the service retains `KillMode=control-group`.
- Only one agent may run per host/user.
  `/run/user/UID/ordo-worker.lock` also binds unfinished attempts to their original state directory across agent stops.
  Never delete this lock or the state directory to bypass reconciliation.
  A clean stop with no unfinished attempts clears the binding, but retains the lock inode.
- Reconnect reuses the service and state.
  A different executable requires explicitly stopping the old service before bootstrap; stopped agents leave tasks alive.
  Transient services need bootstrap after a host reboot.
- Worker commands return JSON.
  `worker get` reports status rather than propagating the task exit code.
- `worker submit --input NAME=DIR` snapshots each named input and mounts it read-only at `/inputs/NAME`.
  Identical snapshots reuse the content-addressed cache.
  Links and special files are rejected.
- Agent tasks write artifacts under `/outputs`.
  These are archived and synced before container cleanup.
  `worker fetch --id ID --output NEW_DIR` downloads verified logs, metadata, and extracted outputs.
  Worker results remain available for repeated downloads.
- Transfers are streamed with SHA-256 verification and atomic publication.
  Each archive is limited to 64 GiB; output files must be readable by the worker user.
  No automatic cache/result deletion is performed.

The Nix package builds with `CGO_ENABLED=0`.
For a macOS controller or different worker architecture, supply a cross-compiled Linux artifact:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ordo-linux-amd64 ./cmd/ordo
```

The opt-in [worker smoke test](tests/README.md) checks real crash/stop/reconnect behavior without enabling services permanently.

## Durable queue

Start `ordo server --state SERVER_DIR --workers workers.json` with 1–10 worker entries:

```json
[{"host":"WORKER","state_dir":"/home/worker/.local/state/ordo","cpus":"0,1,64,65","memory_mib":512,"binary":"/absolute/path/ordo-linux-amd64"}]
```

Submit `ordo queue submit --state SERVER_DIR --file job.json`:

```json
{"id":"evaluation-001","image":"sha256:IMAGE_ID","archive":"/absolute/path/image.tar","inputs":{"dataset":"/absolute/path/data"},"resources":{"cores":1,"memory_mib":256},"args":["-c","cp /inputs/dataset/value /outputs/value"]}
```

Without `archive`, the server resolves and exports `image` from its local Podman storage.
`ordo queue list` and `ordo queue get --id ID` accept the same `--state` argument.
The control socket is private.
Server shutdown/disconnection never cancels attempts; dispatch IDs are committed before submission, and worker identity must match before reconciliation.
A worker losing an acknowledged attempt or changing identity blocks that job rather than rerunning it.
Results must be downloaded and validated before the server marks success.

## Workflows

`ordo queue workflow --state SERVER_DIR --file workflow.json` submits the entire graph atomically:

```json
{"id":"example","image":"sha256:IMAGE_ID","archive":"/absolute/image.tar","tasks":[
  {"id":"produce","resources":{"cores":1,"memory_mib":128},"args":["-c","printf data > /outputs/value"]},
  {"id":"consume","from":{"data":"produce"},"resources":{"cores":1,"memory_mib":128},"args":["-c","cp /inputs/data/value /outputs/value"]},
  {"id":"sweep","depends_on":["produce"],"matrix":{"seed":["1","2"]},"resources":{"cores":1,"memory_mib":128},"timeout_seconds":60,"retry":{"task":1,"infrastructure":1},"args":["-c","printf {{seed}} > /outputs/seed"]}
]}
```

- Workflow IDs are at most 24 characters, task IDs 32; expanded jobs use `WORKFLOW--TASK` IDs.
  A workflow expands to at most 1,000 tasks.
- Matrices form a deterministic Cartesian product of string values; `{{parameter}}` is substituted in command arguments, never evaluated by a host shell.
  Expanded IDs have numeric suffixes starting at `-0`.
- `depends_on` requires every referenced task to succeed.
  A sweep reference creates a barrier across all its instances.
  `from` selects one task's collected output directory and adds its dependency automatically; select an explicit instance for sweep outputs.
- Failed/cancelled dependencies skip downstream tasks; independent branches continue.
- Task-exit retries default to zero, confirmed infrastructure-failure retries to one.
  Each count is bounded to 0–10.
  Every executed attempt remains visible.
  Unknown/disconnected executions do not consume retries.
- `timeout_seconds` is a per-attempt wall-clock limit from worker acceptance, enforced by the worker even when the controller is unavailable.
  Zero disables it; the maximum is one year.
- `ordo queue cancel --id JOB_ID` and `ordo queue cancel-workflow --id WORKFLOW_ID` persist cancellation intent.
  Unknown outcomes stay pending/blocked until reconciled.
  `ordo worker cancel --id ATTEMPT_ID` also supports direct worker control.

## Development

The Nix layout follows [Sermo](https://github.com/r3clab/Sermo): Blueprint assembles outputs from `nix/`, with inputs pinned in `flake.lock`.
Go is pinned through Nixpkgs; the shell also provides `gopls`, `golangci-lint`, and treefmt.
Supported development platforms: x86-64 Linux, ARM64 Linux, and Apple Silicon macOS; execution workers require Linux.

```sh
nix develop                    # or direnv allow
nix fmt                        # Go, Nix, Markdown, YAML, JSON, and spelling
go test -race ./...
golangci-lint run ./...
go run ./cmd/ordo --help
nix build                      # binary at result/bin/ordo
nix flake check                # formatting, lint, build, and race-enabled tests
```

- `nix/devshell.nix`: tools and a fail-fast formatting/lint pre-commit hook, installed on shell entry.
- `nix/formatter.nix`: treefmt configuration and formatting check.
- `nix/package.nix`: Go build, tests, and sandboxed lint check.

Run commits from Ordo's own dev shell so its hooks apply.
Nix's Git-based flakes only see tracked files: add new files to Git before checking, or use `path:.` (for example, `nix develop path:.`) while bootstrapping.

## Still open

- Exact upstream components and tests to reuse or port.
- Hardware-matching fields and enforcement details on rootless workers.
- Task volumes, durations, data sizes, retry limits, timeouts, and downstream-failure behavior.

## License

[MIT](LICENSE) for Ordo's original code.
Upstream code reuse needs a separate license check: Nomad currently uses BUSL-1.1; HyperQueue uses MIT.
