# Ordo overview figure

[Light](overview-light.svg) and [dark](overview-dark.svg) versions of the compact controller-to-worker flow.
`overview.typ` is the editable source; the SVGs contain outlined text and need no fonts or scripts to view.
The detailed [command reference](https://github.com/r3clab/Ordo/wiki/Command-map) lives in the project wiki as text.

## Text equivalent

Start `ordo server` first; it reaches each configured worker over SSH and installs nothing there.
Command labels identify actions, not complete invocations: required flags are omitted.

1. **Submit:** on the server host, `ordo submit` sends JSONL jobs, each naming the `out` directory its results belong in, through the server's private Unix socket.
2. **Prepare and queue:** the server builds or pulls each pinned image, snapshots inputs by content digest, and records the jobs in its durable queue.
3. **Dispatch:** once the jobs it takes inputs from have finished, the server reserves cores, GPUs, and memory on a worker carrying the job's tags, records the attempt, and uploads the image and inputs over SSH, each once per worker.
4. **Run:** `podman run` starts the task in rootless Podman, limited to its reservation.
5. **Collect:** after the container exits, the server streams its logs and outputs back over SSH, records the attempt, and removes the container; a job's next step returns to dispatch.
6. **Deliver:** the server moves the job's outputs into its `out`, with Ordo's record and logs under `out/.ordo/`; `ordo submit --wait` reports how each job ended.

Solid arrows show requests and dispatch; dashed gold arrows show returned results.

Tasks survive SSH disconnects and server restarts, and unknown outcomes block automatic retries.
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
