<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/folio-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="assets/brand/folio-light.svg">
    <img src="assets/brand/folio-light.svg" alt="Ordo" width="360">
  </picture>
</h1>

Ordo schedules reproducible evaluations across SSH-accessible Linux servers.
Each job runs in rootless Podman on exclusive physical cores and GPUs, from a [nix2container](https://github.com/nlewo/nix2container) image or a registry image pinned by digest.
Jobs are plain JSONL, the queue survives server restarts and SSH disconnects, and a local web UI shows progress.

## How it works

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/figures/overview-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="assets/figures/overview-light.svg">
  <img src="assets/figures/overview-light.svg" alt="Ordo's pipeline: submit JSONL jobs; the server prepares pinned images and inputs and queues them, dispatches each job over SSH to a worker that runs it in rootless Podman, collects its logs and outputs, then delivers them into the job's out directory." width="960">
</picture>

`ordo server` drives the workers over SSH; nothing of Ordo is installed on them.
Each job names an `out` directory on the server host, where its results land when it ends.
A non-empty `out` means done: rerunning a sweep queues only what is missing, and deleting an `out` reruns that job.

## Setup

- **Server:** Nix.
  The Nix-built `ordo` bundles nix2container's `skopeo`.
- **Workers:** Linux, rootless Podman with delegated cgroup v2 `cpuset` and `memory` controllers, OpenSSH, a POSIX shell, GNU `tar` and `find`, a user systemd manager, and lingering (`loginctl enable-linger`).
  The server connects by SSH alias, so set `User`, `Port`, and `IdentityFile` in its `~/.ssh/config`; authentication must not prompt.
  Each worker account belongs to one server.

List the workers in `$XDG_CONFIG_HOME/ordo/server.json` (or `--config FILE`), then start `ordo server`:

```json
{"workers":[{"host":"WORKER","cpus":"0,1,64,65","memory_mib":512}]}
```

- `cpus` must include every online SMT sibling of each core; jobs request **physical cores**.
- `memory_mib` is the budget the worker's jobs share.
- Optional per worker: `gpus`, `tags` (see [GPUs and tags](#gpus-and-tags)), and `state_dir` (worker-local, mode 0700, default `~/.local/state/ordo/worker`; set it for a worker unreachable on the server's first start).
- Optional top level: `listen`, `services`, `secrets`, `nix`, `skopeo`.
  Unknown keys are rejected.
- Server state lives in `--state DIR`, else `$ORDO_STATE`, else `$XDG_STATE_HOME/ordo/server`; client commands find the server the same way.

## Jobs

A job is one JSON line; a sweep is a loop:

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

`ordo submit` prints a result per line and exits nonzero if any line failed; `--wait` also reports each job as it ends.

- `out` is required and identifies the job.
  Relative paths resolve from the submitting directory, on the server host.
- A line whose `out` is non-empty is `done` and queues nothing.
  Resubmitting an unfinished job returns it (`existing`); the same `out` with a different request is an error.
- `ordo schema` prints the job format's JSON Schema.

### Results

The task writes to `/outputs`.
When the job ends, the server moves those outputs into `out` atomically, adding `out/.ordo/job.json` (every attempt, its worker, allocation, and exit status) and each step's logs as `STEP.stdout` and `STEP.stderr`.
A failed job still delivers what it wrote; a cancelled or skipped job delivers nothing.

### Steps

```json
{"out":"results/grep/1","inputs":{"bitcode":"bitcode/grep.bc"},"steps":[
  {"name":"klee","image":"/srv/eval#images.klee","resources":{"cores":1,"memory_mib":4096},"args":["-c","klee --output-dir=/outputs/klee /inputs/bitcode"]},
  {"name":"coverage","image":"/srv/eval#images.gcov","resources":{"cores":8,"memory_mib":16384},"args":["-c","replay /outputs/klee > /outputs/coverage.json"]}
]}
```

Up to 32 steps run in order, each in its own container with its own image and resources, starting from the previous step's `/outputs`.
A failed step ends the job; retries and timeouts apply per step.

### Inputs and dependencies

`inputs` maps names to files or directories on the server host, snapshotted at submission and mounted read-only at `/inputs/NAME`.
An input inside another unfinished job's `out` makes the job wait for that one, and skips it if that one fails or is cancelled:

```json
{"out":"results/model","image":"/srv/eval#images.train","resources":{"cores":4,"memory_mib":8192},"args":["-c","train > /outputs/weights"]}
{"out":"results/score","inputs":{"model":"results/model/weights"},"image":"/srv/eval#images.eval","resources":{"cores":1,"memory_mib":2048},"args":["-c","score /inputs/model > /outputs/score"]}
```

### Images

- **Nix installable**, such as `"/srv/eval#images.klee"` or `"/nix/store/….drv^*"`: the server builds it and exports the nix2container image.
  Make Ordo's `nix2container` input follow yours.
  A `go build` binary needs the config's `skopeo` set to nix2container's, since stock `skopeo` lacks the `nix:` transport.
- **Registry image pinned by digest**, `"docker://NAME@sha256:DIGEST"`.
  Tags are rejected.

### Retries, timeouts, stop, and cancel

- `retry.task` (default 0) and `retry.infrastructure` (default 1) each allow 0–10 retries.
  A disconnect never consumes one.
- `timeout_seconds` limits each attempt's wall-clock time; expiry is a task failure.
- `ordo queue stop` sends SIGTERM, then SIGKILL after 10 seconds, and keeps the outputs.
  The step is judged by its exit status and never retried.
- `ordo queue cancel` ends the job `cancelled`, delivers nothing, and skips jobs waiting on it.

### Queue

`ordo queue list`, `get`, `stop`, and `cancel` select jobs by `--out DIR`, `--under DIR`, or `--id ID`.

Jobs move through `queued`, `dispatching`, `running`, `collecting`, and `delivering`, and end `succeeded`, `failed`, `cancelled`, or `skipped`.
Containers keep running through server restarts and disconnects, and the server adopts them again.
When an outcome is unknown, such as a vanished container or a worker whose machine ID changed, the job is `blocked`: it keeps its reservation and is never retried until you cancel it.

## GPUs and tags

- **GPUs:** list a worker's NVIDIA GPU UUIDs in `gpus`, and request a count with a job's `resources.gpus`.
  Each GPU goes to one job at a time through a private CDI spec, which needs `nvidia-smi` and `nvidia-ctk` on the worker.
  A GPU already running a process is not allocated.
- **Tags:** give equivalent workers a shared tag, such as `"tags":["l40s-class"]`, and list it in a job's `tags`.
  The job then runs only on workers carrying all its tags, and waits rather than falling back.

## Job environment

```json
{"env":{"MODE":"eval"},"secrets":["MODEL_TOKEN"],"network":"egress","cap_add":["NET_ADMIN"],"services":["model"]}
```

- `env` values appear in job records and the web UI; `NVIDIA_*` names are reserved.
- `secrets` lists names whose values come from files in the server config, `"secrets":{"NAME":"FILE"}`; the task sees each as `$NAME`.
  Values never enter job records or the API.
  **Anyone who can submit jobs can use every configured secret**, and a task can print one to its logs.
- `network` is `none` (default) or `egress`.
- `cap_add` accepts only `NET_ADMIN`.
- `services` names endpoints in the server config, `"services":{"NAME":"HOST:PORT"}`, forwarded over SSH to `/services/NAME/socket`, even with `network` set to `none`.

## Web UI

The server serves the UI on loopback only, at `http://127.0.0.1:7331` by default (`listen` changes it).
Paste the bearer token from `SERVER_DIR/token`.
For a remote server, forward it with `ssh -L 7331:127.0.0.1:7331 SERVER`; never expose it publicly, since it has no TLS or multi-user authentication.

The UI source is in `ui/`; its build in `internal/server/web/` is committed, so commit `npm run build` output with UI changes.

## Acknowledgements

Ordo borrows design ideas from these projects:

- [Nomad](https://github.com/hashicorp/nomad) and its [Podman driver](https://github.com/hashicorp/nomad-driver-podman):
  - Recovering a running container after a server restart or disconnect, found by the attempt it carries, instead of starting it again.
  - Treating a lost connection as an unknown outcome, not a failure.
  - Keeping task-failure retries apart from infrastructure-failure retries, each bounded.
  - Enforcing reservations with cgroups: whole cores through `cpuset`, and a memory limit.
- [HyperQueue](https://github.com/It4innovations/hyperqueue):
  - Jobs that wait for other jobs, and are skipped when one they wait for fails or is cancelled.
  - Indexed resources: a task holds specific cores and GPUs exclusively, not just a count of them.
  - Per-worker resource accounting, so a job stays queued until a worker has room for it.
