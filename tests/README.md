# Live worker checks

`flake.nix` builds a small nix2container image with Bash and coreutils.
These scripts are opt-in checks against real workers, not part of sandboxed package tests.
They require local `ssh`, `jq`, `nix`, and coreutils, plus the documented worker prerequisites.

Build the server:

```sh
nix build path:. -o result
```

The server builds the test image from `ORDO_TEST_INSTALLABLE`.
`ORDO_TEST_IMAGE` is that image's immutable configuration ID, which the scripts use to clean up workers; `ordo queue get` shows it as `steps[0].spec.image` once the server has built it.
The pinned fixture currently has the ID below; rebuilding with changed flake inputs/configuration can change it.

## Single-session SSH transport

On Linux, run the isolated SSH integration test:

```sh
ORDO_TEST_SSHD=1 go test -race -count=1 -timeout=2m -run '^TestSSHMaxSessions$' ./internal/worker
```

It starts an unprivileged `sshd` on loopback with temporary keys and `MaxSessions 1`, without changing the host's SSH configuration.
It checks automatic helper deployment, cached reconnects, overlapping transfers and control requests, rejection of a second exec session, and cleanup after controller SIGKILL with an active service forward.
Podman commands are faked by default.

To also prove container survival, stopping, and output collection after reconnecting, set `ORDO_TEST_PODMAN_IMAGE` to the full `sha256:` ID of an **already present** image with a shell entrypoint, `echo`, and `sleep` (for example the fixture below).
This variant uses one CPU and 128 MiB, requires the real worker prerequisites on the local host (including delegated cpuset/memory controllers), and removes only its own test container.
It never enables lingering or changes cgroup delegation.

Packaged builds bundle static Linux amd64 and arm64 executables in `libexec/ordo`.
The server deploys the appropriate executable by digest; the helper protocol is checked before exposing its API.
Worker state and container labels retain their existing format, so rolling back the controller does not require a data migration.
Stop the old controller before starting another version against the same worker account.

## Server, jobs, and browser

```sh
ORDO_TEST_BINARY="$PWD/result/bin/ordo" \
ORDO_TEST_INSTALLABLE="path:$PWD/tests#image" \
ORDO_TEST_IMAGE=sha256:868a72e2f666cf6a96edadfc5a339f2655865ab968babde2644c31d88380d85e \
bash tests/server-smoke.sh
```

It uses the workers `vegeta` and `goku`, each with the verified pool `62,63,126,127` (four cores) and a 512 MiB aggregate admission budget.
It checks server SIGKILL/restart without orphaned SSH processes or duplicate executions, delivery into each job's `out` (including `.ordo/`), a job waiting on an input in another job's `out`, a two-step job with different resources per step, bounded retries, skipped dependents, timeout, cancellation, and `ordo submit --wait`.
Add `ORDO_TEST_CHROMIUM=/path/to/chromium` (and Node.js 22+ on PATH) to drive the real UI: status of jobs queued by `ordo submit`, running-job cancellation, and safe log rendering.
Browser logs and a screenshot join the evidence directory.
The headless browser uses a private profile and disabled sandbox only for this local test; no browser/runtime dependency is added to Ordo.
Go tests separately cover cross-origin requests, hostile Host headers, and bounded log reads.

## GPU and tag matching

Set `ORDO_TEST_INSTALLABLE` to the dedicated `path:$PWD/tests#gpu-image` fixture and `ORDO_TEST_IMAGE` to its configuration digest.
With the same environment variables, run `bash tests/gpu-smoke.sh HOST FULL_GPU_UUID` only after checking current compute and graphics processes.
The script captures inventories; dispatch also refuses devices with existing processes.
It uses one GPU, one core, and 256 MiB for a tiny CUDA allocation/set/copy and checks exactly one visible UUID.
A second GPU job stays queued while the first holds the device, submission rejects a tag no worker carries, and a server SIGKILL/restart leaves the GPU job with one attempt.
No sustained GPU workload or global CDI registration is used.

## Cleanup

Successful runs remove their remote state directories and any image they introduced, and check that no container of theirs remains.
Failed runs retain remote state for diagnosis; a bounded task may still be running.
Do not start another run on the host until those resources are reconciled and cleaned up.
Evidence, including the server state and every job's delivered `out`, is kept in a private local `/tmp/ordo-*-evidence.*` directory.

These checks do not prove host-reboot/power-loss recovery, ARM runtime behavior, multi-GPU task execution, or performance under sustained load.
No test enables services permanently or changes host configuration.

## Runtime findings

Earlier live tests exposed two runtime dependencies that the worker scripts still account for:

1. Podman 5.6.2 leaves conmon in the calling cgroup when `INVOCATION_ID` is set.
2. On these ZFS-backed workers, `fuse-overlayfs` also inherits the caller's cgroup; killing it causes task SIGBUS failures even when the container initially appears alive.

Every Podman command therefore runs in its own transient scope (`systemd-run --user --scope`), and the asynchronous stop in a transient service, with literal arguments across old and new systemd expansion behavior, so neither process dies with the SSH session that started it.
