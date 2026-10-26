# Ordo

A task dispatcher and scheduler for fair, reproducible evaluations across SSH-accessible workers.
Every task runs in rootless Podman using a [nix2container](https://github.com/nlewo/nix2container)-built image.

**Status:** Ordo runs local and SSH-managed tasks with exclusive CPU/GPU allocations, hardware matching, a durable queue, JSON workflows, and a loopback web UI.

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

## Web UI

The server also listens on `http://127.0.0.1:7331`; override the port with `--listen 127.0.0.1:PORT`.
Only literal loopback bind addresses are accepted.
Read `SERVER_DIR/token` (mode 0600) and paste its bearer token into the page.
The token is stable across restarts, never placed in a URL/cookie/browser storage, and cleared on disconnect or page reload.

For a remote server, use SSH forwarding, for example `ssh -L 7331:127.0.0.1:7331 SERVER`, then open the local URL.
Do not expose the listener with a public proxy; public deployment/TLS/multi-user authorization are deliberately unsupported.
Authenticated API requests additionally enforce loopback Host headers and same-origin browser requests.

The page accepts job or workflow JSON, shows worker connectivity/hardware/reservations and attempt histories, and cancels jobs or whole workflows.
Image/input paths in submitted JSON refer to the **server's filesystem**.
Logs are available after result collection, with the last 1 MiB of each stream shown; full logs remain in each attempt's result directory.
Untrusted task output is rendered as text, not HTML.
Auto-refresh can be paused.

### Developing the UI

The UI source is a Svelte 5, Vite, and Tailwind CSS app in `ui/`.
Its build output in `internal/server/web/` is generated, embedded into the binary, and committed so `go build` and `nix build` need no Node.js step.
Commit `ui/` changes together with a fresh build:

```sh
cd ui
npm ci
npm run check   # type-check
npm run build   # regenerate internal/server/web/
```

`nix flake check` type-checks the UI and fails if the committed build differs from a fresh one.
After changing `ui/package-lock.json`, update `npmDepsHash` in `nix/checks/ui.nix`.

`npm run dev` serves the UI with hot reload and proxies `/v1` to a server listening on `127.0.0.1:7331`.

## GPU workers

Configure a worker's optional `gpus` array with full NVIDIA GPU UUIDs, or pass `--gpus UUID,UUID` to `worker start` and subsequent worker commands.
Request `resources.gpus` in queue/workflow JSON or `--task-gpus N` with `worker submit`.
An omitted pool disables GPU admission; ordinal indices, `all`, MIG partitions, and unsupported device controls are rejected.

- Agents discover UUID/model/memory/driver information and generate a private CDI specification under `STATE/cdi`, using `nvidia-ctk`.
  No global CDI registration or host configuration change is needed.
- GPU reservations are exclusive and durable.
  Restart rejects missing/changed reserved hardware or drivers.
  Admission checks compute/graphics processes, not merely utilization; unrelated processes starting afterward remain outside Ordo's guarantee.
- Podman receives only allocated CDI devices, with legacy NVIDIA hook visibility disabled.
  No privileged mode or device-isolation fallback is used.
- GPU images must work with the injected driver libraries; NVIDIA CDI supplies `NVIDIA_CTK_LIBCUDA_DIR`.
  The nix2container GPU fixture uses this directory to load `libcuda.so.1`.
- `tests/gpu-smoke.sh` verifies exactly one visible GPU UUID, a tiny CUDA allocation/set/copy, overlap rejection, and agent-crash recovery.
  Run it only against a GPU confirmed free of existing processes.

## Hardware matching

Queue/workflow tasks accept an optional `match` object:

```json
{"match":{"arch":"amd64","cpu_model":"AMD EPYC 9554 64-Core Processor","gpu_model":"NVIDIA L40S","min_gpu_memory_mib":40000,"gpu_driver":"580.178.04"}}
```

Omitted fields are relaxed.
Model/driver strings match exactly; architecture is `amd64` or `arm64`.
GPU constraints require a positive GPU count and filter the devices actually allocated, not merely the host inventory.
Direct worker submissions support equivalent `--arch`, `--cpu-model`, `--gpu-model`, `--min-gpu-memory-mib`, and `--gpu-driver` flags.
The agent independently enforces these constraints, including after restart.
A job with no eligible worker remains queued; it never falls back to weaker hardware.
Matching uses the worker's reported inventory and does not control clocks, thermal state, shared memory bandwidth, or unrelated host activity.

## Task environment

Queue/workflow tasks may declare environment, secrets, network access, capabilities, and controller-side services:

```json
{"env":{"MODE":"eval"},"secrets":{"MODEL_TOKEN":"VALUE"},"network":"egress","cap_add":["NET_ADMIN"],"services":["model"]}
```

- `env` values are part of the job's specification and appear in job records, `run.json`, and the web UI.
  Names starting with `NVIDIA_` are reserved, since NVIDIA's container hooks read them to inject devices.
- `secrets` values are stored apart from jobs in the server's and worker's private state, deleted once the job or attempt ends, and never returned by the API or written to `run.json`, which lists only their names.
  They reach Podman through a mode-0600 env file removed right after container creation, never through argv.
  Values cannot contain NUL or line breaks.
  Secret values are not part of a job's identity: resubmitting an existing ID keeps the original values.
  `podman inspect` on the worker shows them while the container exists.
- `network` is `none` (default, loopback only) or `egress` (rootless pasta with full outbound access).
  Domain filtering belongs in the image, for example with nftables under `NET_ADMIN`.
- `cap_add` accepts only `NET_ADMIN`, which acts on the container's own network namespace under rootless Podman.
- `services` names endpoints configured on the server with repeated `--service NAME=HOST:PORT`, resolved from the server.
  The server's SSH tunnel to each worker forwards each one to a Unix socket, and the task sees it at `/services/NAME/socket`, even with `network` set to `none`.
  The directory is mounted rather than the socket, so a task keeps reaching the service after a tunnel reconnect; while the server is disconnected, connections fail and the task must retry.
  Only the container's root user (the worker user) can connect.
  Clients that need TCP bridge inside the image, for example `socat TCP-LISTEN:8317,bind=127.0.0.1,fork UNIX-CONNECT:/services/model/socket`.
  Workers' sshd must allow stream-local forwarding (`AllowStreamLocalForwarding`, the default).
  The server refuses a configuration that drops a service an unfinished job uses.
- Direct worker submissions support `--env NAME=VALUE`, `--secret NAME` (value read from the CLI's own environment), `--network`, and `--cap-add`, but not services, since their tunnel closes when the command exits.

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
  A cancelled job ends `cancelled` and its dependents are skipped.
  Unknown outcomes stay pending/blocked until reconciled.
  `ordo worker cancel --id ATTEMPT_ID` also supports direct worker control.
- `ordo queue stop --id JOB_ID` ends a running job early, for example when an external budget runs out, but keeps its result.
  The container gets SIGTERM, then SIGKILL after 10 seconds, and the attempt is collected and classified by its exit status like any other exit, so a task that traps SIGTERM and exits zero succeeds and its dependents run.
  A stopped job is never retried; stopping a job before it starts cancels it.
  `ordo worker stop --id ATTEMPT_ID` stops one attempt directly.
- Every stop and cancel accepts `--reason TEXT` (up to 1 KiB), recorded as `stop_reason` on the job and attempt.
  Attempts also record `stop_requested`: `cancel`, `stop`, or `timeout`.
  The first request reaching an attempt wins, but a later cancel still marks the job `cancelled`.

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
nix flake check                # formatting, lint, build, race-enabled tests, and UI build
```

- `nix/devshell.nix`: tools and a fail-fast formatting/lint pre-commit hook, installed on shell entry.
- `nix/formatter.nix`: treefmt configuration and formatting check.
- `nix/package.nix`: Go build, tests, and sandboxed lint check.
- `nix/checks/ui.nix`: UI type check and committed-build freshness check.

CI runs `nix flake check` on every push to `main` and every pull request.

Run commits from Ordo's own dev shell so its hooks apply.
Nix's Git-based flakes only see tracked files: add new files to Git before checking, or use `path:.` (for example, `nix develop path:.`) while bootstrapping.
Run `nix flake check` itself Git-backed: a `path:.` snapshot can include `.git`, which makes the formatter check fail with “nothing to commit”.

## Still open

- Exact upstream components and tests to reuse or port.

## License

[MIT](LICENSE) for Ordo's original code.
Upstream code reuse needs a separate license check: Nomad currently uses BUSL-1.1; HyperQueue uses MIT.
