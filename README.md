<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="assets/brand/folio-dark.svg">
    <source media="(prefers-color-scheme: light)" srcset="assets/brand/folio-light.svg">
    <img src="assets/brand/folio-light.svg" alt="Ordo" width="360">
  </picture>
</h1>

Ordo runs reproducible evaluations across SSH-accessible Linux workers.
Submit jobs as JSONL, run them in rootless Podman, and collect results in local directories.
CPUs and GPUs are allocated exclusively among Ordo jobs.
Running containers survive server restarts and SSH disconnects.

## Worker setup

Workers need Linux, rootless Podman, cgroup v2, and a systemd user manager with lingering enabled.
The server must reach them over SSH without prompts.
No Ordo installation is needed on workers.
See [worker preparation](https://github.com/r3clab/Ordo/wiki/Getting-started#1-prepare-workers) for setup commands.

## Configuration

Install Ordo on the server host with Nix and flakes enabled:

```sh
nix profile add github:r3clab/Ordo
```

Save a config to `~/.config/ordo/server.json`, replacing the example values:

```json
{
  "workers": [
    {"host": "cpu1", "cpus": "0,1,64,65", "memory_mib": 4096}
  ]
}
```

`host` is an SSH host or alias.
`cpus` lists the logical CPU IDs jobs may use, and each counts as one core; list one SMT sibling per physical core to keep jobs off shared physical cores.
`memory_mib` is the worker's total job memory budget.

Start `ordo server` and run CLI commands on the same host.
See [configuration options](https://github.com/r3clab/Ordo/wiki/Configuration-and-jobs#server-configuration) for GPUs, tags, services, and custom paths.

## Jobs

Save this example as `job.json`.
It assumes `data/input.txt` exists and `/srv/eval#images.tools` provides a shell entrypoint, `wc`, and `sha256sum`:

```json
{
  "out": "results/word-count",
  "inputs": {"dataset": "data"},
  "steps": [
    {
      "name": "count",
      "image": "/srv/eval#images.tools",
      "resources": {"cores": 1, "memory_mib": 256},
      "timeout_seconds": 60,
      "args": ["-c", "wc -w < /inputs/dataset/input.txt > /outputs/count.txt"]
    },
    {
      "name": "checksum",
      "image": "/srv/eval#images.tools",
      "resources": {"cores": 1, "memory_mib": 256},
      "timeout_seconds": 60,
      "args": ["-c", "cd /outputs && sha256sum count.txt > count.sha256"]
    }
  ]
}
```

- `out` names the result directory on the server host and identifies the job.
- `inputs` maps server-host directories to read-only mounts under `/inputs`.
- `steps` runs containers in order, sharing `/outputs` between steps.
- `image` accepts a [nix2container](https://github.com/nlewo/nix2container) installable or a registry image pinned as `docker://NAME@sha256:DIGEST`.
- `resources` requests cores (CPUs from the worker's `cpus`), memory in MiB, and an optional GPU count.
- `args` supplies arguments to the image's entrypoint, which an optional `entrypoint` replaces; `workdir` replaces its working directory with one that already exists, such as `/outputs`.

For a single step, put its fields at the top level instead of using `steps`.
Relative paths start where you run `ordo submit`.

Convert the example to JSONL and submit:

```sh
jq -c . job.json > jobs.jsonl
ordo submit --file jobs.jsonl --wait
```

Each line is a job.
`--wait` reports final results and exits nonzero if a submission fails or a tracked job does not succeed.

Results appear in `out`, with job history and logs under `.ordo/`.
The server forgets finished jobs after 30 days.
Task failures still deliver collected outputs, while cancelled and skipped jobs deliver nothing.
A nonempty `out` with no unfinished job counts as done, even after a task failure.
Use a new output path to rerun a finished job.

Inputs under another unfinished job's output directory make the job wait for it.
Failure or cancellation skips dependent jobs.
See [job options](https://github.com/r3clab/Ordo/wiki/Configuration-and-jobs#job-options) for retries, dependencies, environment variables, and secrets.
Submission fails if no worker matching the job's tags is configured with the requested resources.
Run `ordo schema` for the full job format.

## Monitor and control

Open the [web UI](http://127.0.0.1:7331) to view workers, jobs, and collected logs.
For remote access, use [SSH forwarding](https://github.com/r3clab/Ordo/wiki/Command-map#web-ui).

```sh
ordo queue list
ordo queue get --out results/word-count
ordo queue stop --out results/word-count
ordo queue cancel --out results/word-count
```

`stop` ends the running step and keeps its outputs.
`cancel` abandons the job without delivering outputs.
See the [command reference](https://github.com/r3clab/Ordo/wiki/Command-map) for selectors and recovery behavior.

## Acknowledgements

Ordo draws ideas from [Nomad](https://github.com/hashicorp/nomad), its [Podman driver](https://github.com/hashicorp/nomad-driver-podman), and [HyperQueue](https://github.com/It4innovations/hyperqueue).
