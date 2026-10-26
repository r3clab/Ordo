# Remaining implementation

Approved: embedded SQLite, JSON workflows, rootless Podman tasks built with nix2container.
Keep the existing local CLI and run records compatible.
Implement real paths, not placeholder endpoints.
Keep each acceptance item in its own buildable commit where possible; mark it complete only after its checks pass.

## Acceptance gates

- [x] Live local execution and interrupted-run recovery on vegeta and goku (M1).
- [x] Persistent agents behind private SSH-forwarded Unix sockets; durable, idempotent attempt submission (M2).
- [x] Exclusive physical-core allocations and aggregate memory accounting, restored before new work after restart (M2).
- [x] Exclusive GPU allocations and recovery (M6a).
- [x] Immutable image transfer, content-addressed input caching, read-only inputs, and output collection (M3).
- [x] Durable server queue and reconciliation after server/tunnel failure; no retries merely because a worker disconnects (M4).
- [x] JSON workflow validation, dependency readiness, parameter expansion, bounded infrastructure/task retries, timeout, cancellation (M5).
- [x] User-selected hardware matching; rootless GPU isolation validated on vegeta and goku, never silently weakened (M6b).
- [x] Minimal authenticated web UI for submission, status, logs, and cancellation (M7).
- [x] Formatting, lint, race tests, Nix package checks, and multi-host smoke tests (M8).

## Test boundary

Use vegeta and goku as jiezzz through the existing SSH configuration.
At most two physical cores and 512 MiB task memory per host; brief single-GPU tests only after checking existing processes/allocations.
No stress tests, sudo, permanent service enablement, host configuration changes, or cleanup of unrelated resources.
Use private Ordo test directories and uniquely named containers/services.
Workers report rootless Podman 5.6.2, cgroup v2 with cpuset/memory, and user lingering.
GPU checks use private per-agent CDI specifications, without modifying global device registration.

## Implementation defaults

- SQLite is embedded; no database service.
  Local run.json remains an inspectable result record.
- Agent control uses mode-0600 Unix sockets under private directories, forwarded through SSH.
- One agent per host/user; unfinished attempts bind that host to their existing state directory.
- Agent services retain whole-service cleanup; independent transient runtime scopes keep Podman monitors and FUSE helpers alive across agent restarts.
- Worker state/reservations are authoritative for local execution; server state retains every attempt.
- Images are preloaded from nix2container on the server, resolved to immutable IDs, and copied to workers; no registry fallback.
- Inputs are immutable snapshots; outputs and retries have separate attempt directories.
- Ambiguous execution or collection failures retain ownership/resources until reconciled.
- Existing local run/recover commands remain available independently of the server.
