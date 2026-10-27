<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/folio-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="assets/brand/folio-light.svg">
    <img src="assets/brand/folio-light.svg" alt="Ordo" width="360">
  </picture>
</h1>

Ordo makes it easy to schedule reproducible evaluations across SSH-accessible servers.
Exclusive CPU/GPU allocations and worker tags support fair comparisons, while a durable queue, plain JSONL jobs, and a local web UI simplify task management.
Every job runs in rootless Podman, from a [nix2container](https://github.com/nlewo/nix2container)-built image or a registry image pinned by digest.

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/figures/overview-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/figures/overview-light.svg">
  <img src="assets/figures/overview-light.svg" alt="Ordo's managed flow: submit jobs to a durable server, start containers in rootless Podman on workers over SSH, verify returned results, then deliver each job's results into its out directory." width="960">
</picture>

`ordo server` drives the workers over SSH and manages scheduling, dispatch, and result collection; nothing of Ordo is installed on a worker.
Each job names an `out` directory on the server host, and its results land there when it finishes: there is nothing to fetch.
A non-empty `out` means the job is done, so rerunning a sweep only queues what is missing, and deleting an `out` runs that job again.

[Command reference](https://github.com/r3clab/Ordo/wiki/Command-map) · [Figure notes and source](assets/figures/README.md).

## Direction

Build a focused, single-user evaluation runner for an initial pool of **1–10 workers**, not a general-purpose orchestration platform.
Use **Go** for the server and CLI.

Learn from and selectively adapt:

- [Nomad](https://github.com/hashicorp/nomad) and its [Podman driver](https://github.com/hashicorp/nomad-driver-podman): task lifecycle, container recovery, and resource enforcement.
- [HyperQueue](https://github.com/It4innovations/hyperqueue): dependency-aware scheduling, resource accounting, and indexed CPU/GPU allocation.

Reuse small, relevant code paths and their tests; port HyperQueue logic where worthwhile.
Neither upstream runtime is required.
Prefer a small Go implementation over importing Nomad's full control plane or maintaining a Go/Rust split.

## Deployment

- **Server:** has Nix installed (the Nix-built `ordo` bundles nix2container's `skopeo`); owns the queue, scheduling decisions, and durable run records.
- **Workers:** each has rootless Podman installed and runs no Ordo process.
  The server runs each operation as a short shell script over one SSH connection per worker.
- **Jobs:** declare a container image, command, inputs, resource requirements, and the `out` their results belong in.
  Every job runs in **rootless Podman**, never directly on the host.
- **Nix:** provides reproducible tooling and environment builds; Podman remains the task runtime.

Workers must support the resource controls and GPU permissions a task requires.
Unsupported allocations must be rejected, not silently weakened.

## Fair evaluation

Fairness means giving compared runs the same declared hardware resources, not fair-share scheduling between users.

- Assign exclusive CPU cores and GPU devices where requested, with consistent memory budgets.
  Concurrent runs must not share their allocated cores or GPUs.
- Let users define hardware requirements per evaluation with worker tags: a broad tag may suit some experiments; others need a tag shared only by identical machines.
- Pin container images by digest and record code, input, configuration, and seed versions where applicable.
- Preserve worker details, resource assignments, timings, exit status, logs, and outputs.
  Keep failed and retried attempts visible.

Exclusive cores and GPUs do not isolate shared caches, memory bandwidth, or unrelated host activity; these remain measurement risks.

## Jobs and recovery

- Provide a CLI, plain JSONL jobs that any script can write, and a monitoring web UI for your own trusted workloads.
- Support independent jobs, parameter sweeps, jobs of several steps with different resources, and jobs that take inputs from other jobs' results.
- Manage input transfers and output delivery over SSH, caching reusable inputs.
- Keep jobs running through server restarts or SSH disconnects.
  Containers carry their attempt ID, so the server finds and collects them after reconnecting, before retries or later steps.
- Support infrastructure-failure retries and user-defined task retry policies, with bounded attempts.
  A disconnect alone is not proof that a task failed.

## Workers

Workers require Linux, rootless Podman with delegated cgroup v2 `cpuset` and `memory` controllers, OpenSSH, a POSIX login shell, GNU `tar` and `find`, a user systemd manager with `systemd-run`, and lingering enabled for the worker user (`loginctl enable-linger`).
Nothing of Ordo is installed there: the server runs each operation as a short shell script over one SSH connection per worker, and Podman's own `conmon` keeps containers running between them.
Use a worker-local filesystem for the state directory, not a shared network filesystem; it must be mode 0700.

- The server reaches each worker by its SSH alias, so set `User`, `Port`, and `IdentityFile` in the server's `~/.ssh/config`.
  Authentication must not prompt.
- Each Podman command runs in its own transient user scope (`systemd-run --user --scope`), and a stop runs as a transient user service, so neither ends with the SSH session that started it.
- The CPU pool must contain every online SMT sibling of each selected physical core.
  Requests count **physical cores**, not logical CPUs.
- Ordo's attempts cannot overlap physical cores; their memory limits must fit the configured aggregate budget.
  Unrelated host processes are outside this guarantee.
- Each container is named `ordo-ATTEMPT` and labeled with its attempt and the server's ID.
  The server records an attempt before launching it, and the name allows one container per attempt, so a lost response or a server restart never starts a task twice.
- One server must own each worker account.
  Ordo containers labeled by another server stop the worker from taking new jobs.
- Images are loaded once into the worker user's Podman storage, and inputs are unpacked once under the state directory by content digest.
  Inputs mount read-only at `/inputs/NAME`; links and special files are rejected.
- Jobs write artifacts under `/outputs`.
  After exit, the server streams logs and outputs back and records the attempt, and only then removes the container.
  The run directory stays on the worker while a later step of the job may start there, so that step begins from those outputs without another transfer; it is removed once the job moves on or ends.
  Outputs may contain only regular files and directories; anything else blocks the job until it is cancelled.
- A worker reboot fails a running attempt as an infrastructure failure.
  A changed `/etc/machine-id` blocks the attempt instead of adopting another machine's state.
- Cached images and inputs are never deleted automatically.

The opt-in [live checks](tests/README.md) cover server restarts, steps, path dependencies, delivery, and GPU allocation on real workers.

## Durable queue

Start `ordo server`; it reads `$XDG_CONFIG_HOME/ordo/server.json` (`--config FILE` overrides it), which lists 1–10 workers:

```json
{"workers":[{"host":"WORKER","cpus":"0,1,64,65","memory_mib":512}]}
```

- The server keeps its state in `SERVER_DIR`: `--state DIR`, else `$ORDO_STATE`, else `$XDG_STATE_HOME/ordo/server`.
  Every other command finds the server the same way; set `ORDO_STATE` to point a shell at another server.
- A worker's `state_dir` defaults to `~/.local/state/ordo/worker` in the worker user's home.
  The server asks for that home over SSH on first start and remembers it, so a later restart works while the worker is down; set `state_dir` for a worker unreachable on first start.
- Optional keys: `listen` (web UI address, default `127.0.0.1:7331`), `services` and `secrets` (see [job environment](#job-environment)), `nix`, and `skopeo`.
  Unknown keys are rejected.

## Jobs

A job is a JSON object, one per line.
Any generator may write them; a parameter sweep is a loop:

```sh
for program in grep sed; do
  for rep in 1 2 3; do
    jq -nc --arg program "$program" --arg rep "$rep" '{
      out: "results/\($program)/\($rep)",
      image: "/srv/eval#images.klee",
      inputs: {bitcode: "bitcode/\($program).bc"},
      resources: {cores: 1, memory_mib: 4096},
      timeout_seconds: 3600,
      args: ["-c", "klee --output-dir=/outputs/klee /inputs/bitcode"]
    }'
  done
done > jobs.jsonl
ordo submit --file jobs.jsonl --wait
```

`ordo submit` reads one job per line from `--file` or stdin and prints one result per line: `line`, `out`, `id`, `state`, and `done`, `existing`, or `error`.
It keeps going past bad lines and exits nonzero if any line failed.

- `out` is required and names the job.
  Relative `out` and input paths are taken from the directory `ordo submit` runs in, and the server resolves them on its host.
- A line whose `out` already exists and is not empty is `done`: nothing is queued and no image is built.
- Resubmitting a line whose job is still unfinished returns that job (`existing`); the same `out` with a different request is an error.
  An `out` inside another unfinished job's `out` is rejected.
- `--wait` then prints each queued or existing job's line again as it ends, with its final `state` and `error`, and fails if any did not succeed.
- Only the private control socket accepts submissions; the web UI's API has none, since a submission can build Nix code, read server files, and use every configured secret.

### Delivery

When a job's last step ends, the server moves its outputs into `out`, adding Ordo's own files under `out/.ordo/`:

- `job.json`: the job's full record, including its final `state`, every attempt, the worker and allocation each ran with, and their exit status.
- `STEP.stdout` and `STEP.stderr`: each step's logs, named by the step's `name` or its 1-based position.

Outputs are unpacked into a temporary directory beside `out`, then renamed into place, so `out` appears complete or not at all.
An `out` made ahead of time must be empty; one that holds files keeps the job `delivering` until it is cleared.
Outputs that contain `.ordo` fail the job.

A job whose task fails still delivers what it wrote, with `job.json` recording `failed`, so its partial results and logs can be inspected; delete `out` to run it again.
A cancelled or skipped job delivers nothing.

### Steps

A job's `steps` run one after another, each in its own container with its own image and resources, and each starting from the outputs the previous step left in `/outputs`:

```json
{"out":"results/grep/1","inputs":{"bitcode":"bitcode/grep.bc"},"steps":[
  {"name":"klee","image":"/srv/eval#images.klee","resources":{"cores":1,"memory_mib":4096},"timeout_seconds":3600,"args":["-c","klee --output-dir=/outputs/klee /inputs/bitcode"]},
  {"name":"coverage","image":"/srv/eval#images.gcov","resources":{"cores":8,"memory_mib":16384},"args":["-c","replay /outputs/klee > /outputs/coverage.json"]}
]}
```

- A job without `steps` is a single step whose fields sit at the top level.
  A job has at most 32 steps; every step sees the job's inputs.
- A step that fails ends the job, which delivers that step's outputs.
- A later step prefers the worker that ran the previous one and kept its outputs, for up to a minute; after that it runs wherever it fits, and the server sends the outputs it collected.
- Retries and timeouts apply to each step on its own.

### Inputs and dependencies

`inputs` maps a name to a file or directory on the server host, mounted read-only at `/inputs/NAME`.
Inputs are snapshotted on submission and stored by content digest; links and special files are rejected.

An input that lies inside another unfinished job's `out` makes this job wait for that one, in the same batch or an earlier one:

```json
{"out":"results/model","image":"/srv/eval#images.train","resources":{"cores":4,"memory_mib":8192},"args":["-c","train > /outputs/weights"]}
{"out":"results/score","inputs":{"model":"results/model/weights"},"image":"/srv/eval#images.eval","resources":{"cores":1,"memory_mib":2048},"args":["-c","score /inputs/model > /outputs/score"]}
```

The input is snapshotted once the job it waits for succeeds; if that job fails or is cancelled, the waiting job is `skipped`.
An input inside an `out` that is already delivered is an ordinary input.

### Images

`image` is a Nix installable or a registry image:

- A Nix installable, such as a derivation `"/nix/store/….drv^*"` or `"/path/flake#images.grep"`: the server runs `nix build`, exports the nix2container image with nix2container's `skopeo`, and records the build as `image_origin`.
  Exports are cached by store path, so a resubmission repeats only the already-built `nix build`.
  The server finds `nix` on its `PATH` (the config's `nix` overrides it) and uses the nix2container `skopeo` bundled by the Nix build (the config's `skopeo` overrides it).
  Stock `skopeo` lacks the `nix:` transport, so a `go build` binary needs `skopeo` pointing at nix2container's.
  Consumers should make Ordo's `nix2container` input follow their own, so the server's `skopeo` reads their image format.
- A registry image pinned by digest, `"docker://NAME@sha256:DIGEST"`: the server pulls it once with `skopeo` and caches it by that reference.
  A tag alone is rejected, since it could name different images over time.

A batch prepares its distinct images concurrently, at most four at once across the server, and a failed image fails only the jobs that use it.

### Retries, timeouts, stop, and cancel

- Task-exit retries (`retry.task`) default to zero, confirmed infrastructure-failure retries (`retry.infrastructure`) to one.
  Each count is bounded to 0–10.
  Every executed attempt remains visible.
  Unknown/disconnected executions do not consume retries.
- `timeout_seconds` is a per-attempt wall-clock limit from container start.
  The server stops an expired container; while the server is unreachable, Podman kills it 60 seconds past the limit.
  Either way the attempt counts as a task failure.
  Zero disables it; the maximum is one year.
- `ordo queue cancel` persists cancellation intent.
  A cancelled job ends `cancelled`, delivers nothing, and jobs waiting on it are skipped.
  Unknown outcomes stay blocked until cancelled.
- `ordo queue stop` ends a running step early, for example when an external budget runs out, but keeps its outputs.
  The container gets SIGTERM, then SIGKILL after 10 seconds, and the attempt is collected and classified by its exit status like any other exit, so a step that traps SIGTERM and exits zero succeeds and the job goes on to its next step.
  A stopped step is never retried; stopping a job that is not running cancels it.
- Every stop and cancel accepts `--reason TEXT` (up to 1 KiB), recorded as `stop_reason` on the job and attempt.
  Attempts also record `stop_requested`: `cancel`, `stop`, or `timeout`.
  The first request reaching an attempt wins, but a later cancel still marks the job `cancelled`.

### Queue

`ordo queue list`, `get`, `stop`, and `cancel` select jobs with `--out DIR`, `--under DIR` (every job whose `out` lies in `DIR`), or `--id ID`; `list` without one shows every job.
Every client command accepts `--state` like the server, and talks to its private control socket there.

A job moves through `queued`, `dispatching`, `running`, `collecting`, and `delivering`, and ends `succeeded`, `failed`, `cancelled`, or `skipped`; `blocked` waits for a cancel.
Server shutdown or disconnection never stops containers; attempts are committed before launch, and the worker's machine ID must match before reconciliation.
A container that disappears or cannot be collected, or a worker whose identity changed, blocks that job rather than rerunning it.
A blocked job keeps its reservation and is never retried; cancel it to release it.
Results must be collected and recorded before the server marks success.

## Web UI

The server also listens on `http://127.0.0.1:7331`; override it with the config's `listen`, such as `"listen":"127.0.0.1:PORT"`.
Only literal loopback bind addresses are accepted.
Read `SERVER_DIR/token` (mode 0600) and paste its bearer token into the page.
The token is stable across restarts, never placed in a URL/cookie/browser storage, and cleared on disconnect or page reload.

For a remote server, use SSH forwarding, for example `ssh -L 7331:127.0.0.1:7331 SERVER`, then open the local URL.
Do not expose the listener with a public proxy; public deployment/TLS/multi-user authorization are deliberately unsupported.
Authenticated API requests additionally enforce loopback Host headers and same-origin browser requests.

The page shows worker connectivity/hardware/reservations, each job's steps, the jobs it waits for, and attempt histories, and cancels jobs; queue work with `ordo submit`.
Logs are available after result collection, with the last 1 MiB of each stream shown; full logs are delivered to each job's `out/.ordo/`.
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

Configure a worker's optional `gpus` array with full NVIDIA GPU UUIDs.
Request them with a job's `resources.gpus`.
An omitted pool disables GPU admission; ordinal indices, `all`, MIG partitions, and unsupported device controls are rejected.

- The server reads UUID/model/memory information with `nvidia-smi` and generates a private CDI specification under `STATE/cdi` with `nvidia-ctk` on the worker.
  No global CDI registration or host configuration change is needed.
- GPU reservations are exclusive and durable.
  A configured GPU missing from the worker keeps that worker offline.
  Admission checks compute/graphics processes, not merely utilization; unrelated processes starting afterward remain outside Ordo's guarantee.
- Podman receives only allocated CDI devices, with legacy NVIDIA hook visibility disabled.
  No privileged mode or device-isolation fallback is used.
- GPU images must work with the injected driver libraries; NVIDIA CDI supplies `NVIDIA_CTK_LIBCUDA_DIR`.
  The nix2container GPU fixture uses this directory to load `libcuda.so.1`.
- `tests/gpu-smoke.sh` verifies exactly one visible GPU UUID, a tiny CUDA allocation/set/copy, exclusive allocation, tag matching, and server-restart recovery.
  Run it only against a GPU confirmed free of existing processes.

## Worker tags

Give the workers you consider equivalent a shared tag, such as `"tags":["l40s-class"]` in each of their config entries, then require it in the job's `tags`:

```json
{"out":"results/train","image":"/srv/eval#images.train","resources":{"cores":1,"gpus":2,"memory_mib":8192},"tags":["l40s-class"]}
```

- A job runs only on workers carrying every tag it lists; jobs without tags still run on any worker.
- Submission rejects tags that no single configured worker carries together.
- A job whose tagged workers are full remains queued; it never falls back to an untagged worker.
- A tag describes the whole worker, including every GPU in its `gpus` list.
- Tags are server policy kept in the server config; changing them needs a server restart.

## Job environment

Jobs, or each of their steps, may declare environment, secrets, network access, capabilities, and controller-side services:

```json
{"env":{"MODE":"eval"},"secrets":["MODEL_TOKEN"],"network":"egress","cap_add":["NET_ADMIN"],"services":["model"]}
```

- `env` values are part of the job's specification and appear in job records, `job.json`, and the web UI.
  Names starting with `NVIDIA_` are reserved, since NVIDIA's container hooks read them to inject devices.
- `secrets` lists names only, so job files hold no values.
  The server supplies them from its config's `"secrets":{"NAME":"FILE"}`, where a relative `FILE` is beside the config, and a task listing `NAME` sees the file's content as `$NAME`.
  Each file is read once at startup, without one trailing newline; a value cannot be empty or contain NUL or line breaks, and is at most 32 KiB.
  Values stay in the server's memory, never in job records, the API, or `job.json`, which lists only names.
  Submission rejects a name the server was not given, and the server refuses to start without a secret that an unfinished job uses.
  Changing a value needs a server restart; later launches receive the new one.
  At launch they travel over SSH standard input into a mode-0600 env file on the worker, removed as soon as `podman run` returns, never through argv.
  `podman inspect` on the worker shows them while the container exists.
  **Anyone who can submit jobs can use every configured secret**, and a task can print one to the logs the web UI shows.
- `network` is `none` (default, loopback only) or `egress` (rootless pasta with full outbound access).
  Domain filtering belongs in the image, for example with nftables under `NET_ADMIN`.
- `cap_add` accepts only `NET_ADMIN`, which acts on the container's own network namespace under rootless Podman.
- `services` names endpoints in the server config's `"services":{"NAME":"HOST:PORT"}`, resolved from the server.
  The server's SSH connection to each worker forwards each one to a Unix socket, and the task sees it at `/services/NAME/socket`, even with `network` set to `none`.
  The directory is mounted rather than the socket, so a task keeps reaching the service after a reconnect; while the server is disconnected, connections fail and the task must retry.
  Only the container's root user (the worker user) can connect.
  Clients that need TCP bridge inside the image, for example `socat TCP-LISTEN:8317,bind=127.0.0.1,fork UNIX-CONNECT:/services/model/socket`.
  Workers' sshd must allow stream-local forwarding (`AllowStreamLocalForwarding`, the default).
  The server refuses a configuration that drops a service an unfinished job uses.

## Wire format

`ordo submit` reads one job per line.
Its fields are snake_case (`out`, `inputs`, `steps`, `resources.memory_mib`, `timeout_seconds`, `cap_add`, `retry`).
`ordo schema` prints its JSON Schema, generated from the Go types; `schema/` holds a committed copy for editors and other tools.
The schema covers names, enums, and limits; rules spanning fields, such as input conflicts, are checked only on submission.

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
- `nix/package.nix`: Go build with nix2container's `skopeo` baked in, tests, and sandboxed lint check.
- `nix/checks/ui.nix`: UI type check and committed-build freshness check.
- `nix/checks/schema.nix`: fails when `schema/` differs from `ordo schema --out`; regenerate with `go run ./cmd/ordo schema --out schema`.

CI runs `nix flake check` on every push to `main` and every pull request.

Run commits from Ordo's own dev shell so its hooks apply.
Nix's Git-based flakes only see tracked files: add new files to Git before checking, or use `path:.` (for example, `nix develop path:.`) while bootstrapping.
Run `nix flake check` itself Git-backed: a `path:.` snapshot can include `.git`, which makes the formatter check fail with “nothing to commit”.

## Still open

- Exact upstream components and tests to reuse or port.

## License

[MIT](LICENSE) for Ordo's original code.
Upstream code reuse needs a separate license check: Nomad currently uses BUSL-1.1; HyperQueue uses MIT.
