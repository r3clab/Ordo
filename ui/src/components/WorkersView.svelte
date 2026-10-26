<script lang="ts">
  import { memory, relative } from "../lib/format";
  import { session } from "../lib/session.svelte";
  import Meter from "./Meter.svelte";
  import StateBadge from "./StateBadge.svelte";

  const workers = $derived(
    session.workers.map((w) => {
      const hardware = w.info?.hardware;
      const held = w.info?.held ?? [];
      return {
        ...w,
        hardware,
        cpus: hardware?.cores?.flat().length ?? 0,
        heldCPUs: held.reduce((n, a) => n + (a.cpus?.length ?? 0), 0),
        heldMemory: held.reduce((n, a) => n + a.memory_mib, 0),
        heldGPUs: new Set(held.flatMap((a) => a.gpus ?? [])),
        reservations: held.filter((a) => a.cpus?.length).length,
      };
    }),
  );
</script>

<div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
  {#each workers as w (w.host)}
    <section class="card flex flex-col gap-4 p-5">
      <header class="flex items-start justify-between gap-3">
        <div class="min-w-0">
          <h2 class="truncate font-semibold">{w.host}</h2>
          <p class="muted truncate text-sm">{w.hardware ? `${w.hardware.hostname} · ${w.hardware.arch}` : "Not yet connected"}</p>
        </div>
        <StateBadge state={w.connected ? "connected" : "offline"} />
      </header>

      {#if w.error || w.info?.error}
        <p class="alert font-mono text-xs">{w.error || w.info?.error}</p>
      {/if}

      {#if w.hardware}
        <div class="space-y-3">
          <Meter label="CPUs reserved" value={w.heldCPUs} max={w.cpus} detail="{w.heldCPUs} / {w.cpus}" />
          <Meter label="Memory reserved" value={w.heldMemory} max={w.hardware.memory_mib} detail="{memory(w.heldMemory)} / {memory(w.hardware.memory_mib)}" />
          {#if w.hardware.gpus?.length}
            <Meter label="GPUs reserved" value={w.heldGPUs.size} max={w.hardware.gpus.length} detail="{w.heldGPUs.size} / {w.hardware.gpus.length}" />
          {/if}
        </div>
        <dl class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 border-t border-zinc-100 pt-4 text-sm dark:border-zinc-800">
          <dt class="muted">CPU</dt>
          <dd class="truncate" title={w.hardware.cpu_model}>{w.hardware.cpu_model || "—"}</dd>
          <dt class="muted">Cores</dt>
          <dd>{w.hardware.cores?.length ?? 0} physical · {w.cpus} logical</dd>
          {#each w.hardware.gpus ?? [] as gpu (gpu.uuid)}
            <dt class="muted">GPU {gpu.index}</dt>
            <dd class="truncate" title={gpu.uuid}>{gpu.model} · {memory(gpu.memory_mib)}{w.heldGPUs.has(gpu.uuid) ? " · reserved" : ""}</dd>
          {/each}
          <dt class="muted">Reservations</dt>
          <dd>{w.reservations}</dd>
        </dl>
      {/if}

      <p class="mt-auto text-xs text-zinc-400">Last seen {relative(w.last_seen)}</p>
    </section>
  {/each}
</div>
