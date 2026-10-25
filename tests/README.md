# Live worker checks

`flake.nix` builds a small nix2container image with Bash and coreutils.
`worker-smoke.sh` is an opt-in lifecycle check, not part of sandboxed package tests.
It requires local `ssh`, `scp`, `rsync`, `jq`, and coreutils, plus the documented worker prerequisites.

Prepare a static Linux binary and the image's OCI archive, for example:

```sh
nix build path:. -o result
nix run path:./tests#image.copyTo -- "oci-archive:/tmp/ordo-smoke.tar:localhost/ordo-smoke:test"
```

Supply the immutable image configuration ID produced by the test flake (or inspect the archive after loading it locally):

```sh
ORDO_TEST_BINARY="$PWD/result/bin/ordo" \
ORDO_TEST_ARCHIVE=/tmp/ordo-smoke.tar \
ORDO_TEST_IMAGE=sha256:868a72e2f666cf6a96edadfc5a339f2655865ab968babde2644c31d88380d85e \
bash tests/worker-smoke.sh vegeta
```

Repeat for `goku`.
The pinned fixture currently has the ID above; rebuilding with changed flake inputs/configuration can change it.
The script uses the verified pool `62,63,126,127` (two physical cores) and a 512 MiB aggregate admission budget.
Its main sleeping task uses two cores and 256 MiB, checks actual cgroup limits, and exits 7.
A subsequent one-core, 384 MiB task checks memory overcommit rejection while a CPU core remains free.
There is no GPU workload, sustained stress, sudo, host configuration change, or permanent service enablement.

Assertions cover:

- Durable idempotent submissions and rejection of overlapping physical-core requests or aggregate memory overcommit.
- Rejection of a second agent with another state directory, both while the original agent runs and while it is stopped with unfinished work.
- Client tunnel teardown, reconnection, agent SIGKILL/automatic restart, and explicit service stop/bootstrap.
- Original container identity, restored reservations, exit code 7, separate stdout/stderr, and one execution only.
- `cpuset.cpus.effective`, `memory.max`, zero swap, final resource release, and completed runtime-scope cleanup.

On success it stops its service and removes its own containers, newly introduced image, and remote test directory.
The empty host/user ownership lock inode intentionally remains; never unlink it.
Evidence is copied into a private local `/tmp/ordo-worker-evidence.*` directory.
On failure it stops its agent and retains remote state for diagnosis; the bounded task may still be running.
Do not start another run on the host until those resources are reconciled and cleaned up.

## M2 findings incorporated

The live tests exposed two independent runtime dependencies:

1. Podman 5.6.2 leaves conmon in the calling cgroup when `INVOCATION_ID` is set.
2. On these ZFS-backed workers, `fuse-overlayfs` also inherits the caller's cgroup; killing it causes task SIGBUS failures even when the container initially appears alive.

The implementation therefore uses separate transient runtime scopes, not `KillMode=process` on the agent.
Unit tests also cover literal command arguments across old/new systemd expansion behavior, ambiguous launch failure, state-write failure, topology changes, and restart ownership.

These checks do not prove host-reboot recovery, power-loss durability, GPU isolation, server reconciliation, image/input transfers, or workflow scheduling.
