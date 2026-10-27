# Live worker checks

`flake.nix` builds a small nix2container image with Bash and coreutils.
These scripts are opt-in checks against real workers, not part of sandboxed package tests.
They require local `ssh`, `jq`, and coreutils, plus the documented worker prerequisites.

Build Ordo and the image's OCI archive, for example:

```sh
nix build path:. -o result
nix run path:./tests#image.copyTo -- "oci-archive:/tmp/ordo-smoke.tar:localhost/ordo-smoke:test"
```

Supply the immutable image configuration ID produced by the test flake (or inspect the archive after loading it locally).
The pinned fixture currently has the ID below; rebuilding with changed flake inputs/configuration can change it.

## Server, workflows, and browser

```sh
ORDO_TEST_BINARY="$PWD/result/bin/ordo" \
ORDO_TEST_ARCHIVE=/tmp/ordo-smoke.tar \
ORDO_TEST_IMAGE=sha256:868a72e2f666cf6a96edadfc5a339f2655865ab968babde2644c31d88380d85e \
bash tests/server-smoke.sh
```

It uses the workers `vegeta` and `goku`, each with the verified pool `62,63,126,127` (two physical cores) and a 512 MiB aggregate admission budget.
It checks server SIGKILL/restart without orphaned SSH processes or duplicate executions, DAG outputs, sweep expansion, bounded retries, skipped dependents, timeout, and cancellation.
Add `ORDO_TEST_CHROMIUM=/path/to/chromium` (and Node.js 22+ on PATH) to drive the real UI: authentication, workflow submission, status, running-task cancellation, safe log rendering, and disconnect.
Browser logs and a screenshot join the evidence directory.
The headless browser uses a private profile and disabled sandbox only for this local test; no browser/runtime dependency is added to Ordo.
Go tests separately cover missing/invalid bearer tokens, cross-origin requests, hostile Host headers, token permissions, and bounded log reads.

## GPU and hardware matching

Build the dedicated `path:./tests#gpu-image.copyTo` fixture as an OCI archive and supply its configuration digest as `ORDO_TEST_IMAGE`.
With the same environment variables, run `bash tests/gpu-smoke.sh HOST FULL_GPU_UUID` only after checking current compute and graphics processes.
The script captures inventories; dispatch also refuses devices with existing processes.
It uses one GPU, one physical core, and 256 MiB for a tiny CUDA allocation/set/copy and checks exactly one visible UUID.
A second GPU job stays queued while the first holds the device, a job matching other hardware never dispatches, and a server SIGKILL/restart leaves the GPU job with one attempt.
No sustained GPU workload or global CDI registration is used.

## Cleanup

Successful runs remove their remote state directories and any image they introduced, and check that no container of theirs remains.
Failed runs retain remote state for diagnosis; a bounded task may still be running.
Do not start another run on the host until those resources are reconciled and cleaned up.
Evidence, including the server state with every collected result, is kept in a private local `/tmp/ordo-*-evidence.*` directory.

These checks do not prove host-reboot/power-loss recovery, ARM runtime behavior, multi-GPU task execution, or performance under sustained load.
No test enables services permanently or changes host configuration.

## Runtime findings

Earlier live tests exposed two runtime dependencies that the worker scripts still account for:

1. Podman 5.6.2 leaves conmon in the calling cgroup when `INVOCATION_ID` is set.
2. On these ZFS-backed workers, `fuse-overlayfs` also inherits the caller's cgroup; killing it causes task SIGBUS failures even when the container initially appears alive.

Every Podman command therefore runs in its own transient scope (`systemd-run --user --scope`), and the asynchronous stop in a transient service, with literal arguments across old and new systemd expansion behavior, so neither process dies with the SSH session that started it.
