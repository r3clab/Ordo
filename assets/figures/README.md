# Ordo workflow figure

[Light](overview-light.svg) and [dark](overview-dark.svg) versions of the compact controller-to-worker flow.
`overview.typ` is the editable source; the SVGs contain outlined text and need no fonts or scripts to view.
The detailed [command reference](https://github.com/r3clab/Ordo/wiki/Command-map) lives in the project wiki as text.

## Text equivalent

Start `ordo server` first; it bootstraps or reuses the configured worker agents.
Command labels identify actions, not complete invocations: required flags are omitted.

1. **Submit:** on the controller, `ordo submit` sends JSONL jobs or `ordo queue workflow` sends a dependency graph through the server's private Unix socket.
2. **Schedule:** the server owns a durable queue, prepares pinned images and inputs, and dispatches eligible work over SSH when dependencies and resource requirements are satisfied.
3. **Execute:** each worker's persistent `ordo agent` reserves the requested cores, GPUs, and memory budget, then runs tasks in rootless Podman.
4. **Collect and verify:** the agent saves logs, outputs, and metadata; the server retrieves and verifies them before marking success.
5. **Fetch:** `ordo fetch --wait` waits for selected jobs and copies succeeded jobs into the chosen results directory.

Solid arrows show requests and dispatch; dashed gold arrows show returned results through the agent.
The server automatically launches agents, so `ordo agent` is not normally a separate manual setup step.
Direct-worker and local-task modes are separate paths, documented in the wiki.

Tasks survive tunnel disconnects, and unknown outcomes block automatic retries.
Exclusive requested cores/GPUs do not isolate shared caches, memory bandwidth, or unrelated host activity.

## Regenerate

Run from the repository root.
Only generation needs Typst, GNU FreeFont, and TeX Gyre fonts; these are not application dependencies.
The figure was generated with Typst 0.15.1, FreeFont 20120503, and TeX Gyre 2.501.

```sh
export TYPST_FONT_PATHS="$(nix build --no-link --print-out-paths \
  nixpkgs#freefont_ttf nixpkgs#gyre-fonts | paste -sd: -)"
for theme in light dark; do
  nix run nixpkgs#typst -- compile --ignore-system-fonts --input theme="$theme" \
    assets/figures/overview.typ "assets/figures/overview-$theme.svg"
done
```

For a raster preview, use a `.png` output path and `--ppi 144`.
