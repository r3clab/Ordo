# Remaining implementation

Approved: embedded SQLite, JSON workflows, rootless Podman tasks built with nix2container.
Keep the existing local CLI and run records compatible.
Implement real paths, not placeholder endpoints.

## Acceptance gates

- [ ] Live local execution and interrupted-run recovery on vegeta and goku.
- [ ] Persistent agents behind private SSH-forwarded Unix sockets; durable, idempotent attempt submission.
- [ ] Exclusive physical-core/GPU allocations and aggregate memory accounting, restored before new work after restart.
- [ ] Immutable image transfer, content-addressed input caching, read-only inputs, and output collection.
- [ ] Durable server queue and reconciliation after server/tunnel failure; no retries merely because a worker disconnects.
- [ ] JSON workflow validation, dependency readiness, parameter expansion, bounded infrastructure/task retries, timeout, cancellation.
- [ ] User-selected hardware matching; rootless GPU isolation validated or rejected, never silently weakened.
- [ ] Minimal authenticated web UI for submission, status, logs, and cancellation.
- [ ] Formatting, lint, race tests, Nix package checks, and multi-host smoke tests.

## Test boundary

Use vegeta and goku as jiezzz through the existing SSH configuration.
At most two physical cores and 512 MiB task memory per host; brief single-GPU tests only after checking existing processes/allocations.
No stress tests, sudo, permanent service enablement, host configuration changes, or cleanup of unrelated resources.
Use private Ordo test directories and uniquely named containers/services.
Workers report rootless Podman 5.6.2, cgroup v2 with cpuset/memory, and user lingering.
Vegeta currently reports no registered NVIDIA CDI devices; investigate a private per-process CDI specification rather than modifying system configuration.

## Implementation defaults

- SQLite is embedded; no database service.
  Local run.json remains an inspectable result record.
- Agent control uses mode-0600 Unix sockets under private directories, forwarded through SSH.
- Worker state/reservations are authoritative for local execution; server state retains every attempt.
- Images are preloaded from nix2container on the server, resolved to immutable IDs, and copied to workers; no registry fallback.
- Inputs are immutable snapshots; outputs and retries have separate attempt directories.
- Ambiguous execution or collection failures retain ownership/resources until reconciled.
- Existing local run/recover commands remain available independently of the server.
