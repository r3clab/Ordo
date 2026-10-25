# Ordo

A task dispatcher and scheduler for fair, reproducible evaluations across SSH-accessible workers.
Every task runs in rootless Podman using a [nix2container](https://github.com/nlewo/nix2container)-built image.

**Status:** Local CPU-only execution/recovery and the worker agent core are implemented.
Agents provide durable, idempotent attempts, exclusive physical cores, and aggregate memory accounting.
SSH bootstrap/control, the server scheduler, transfers, workflows, GPU allocation, and web UI are still planned.

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

## Worker agent core

`ordo agent --state DIR --cpus CPU_IDS --memory-mib MIB` serves a private mode-0600 Unix socket at `DIR/agent.sock` on Linux.
The allowed CPU pool must contain every online SMT sibling of each selected physical core.
Requests count physical cores; memory is an aggregate admission budget with per-container limits and no swap.
Images must already be loaded, and requests use immutable SHA-256 image IDs.

- SQLite commits attempts and reservations before launch; restart restores ownership before admission.
- Repeating an attempt ID with the same specification is idempotent; changing its specification conflicts.
- Unknown outcomes remain blocked with resources held, never silently retried.
- One agent may run per host/user. `/run/user/UID/ordo-worker.lock` binds unfinished work to the original state directory, including while the agent is stopped. Never delete the lock to bypass reconciliation.
- Podman calls launched from a systemd service use separate transient runtime scopes, so container monitors and FUSE mounts survive agent service cleanup.
- Inspectable results and logs remain in `DIR/runs/ATTEMPT`; stopping the agent does not cancel task containers.
- Standalone runs and unrelated host processes are outside the agent's exclusivity guarantee.

SSH deployment and a client for this private agent API are the next acceptance item.

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
