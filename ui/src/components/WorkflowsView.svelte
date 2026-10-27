<script lang="ts">
  import type { Job, JobState } from "../lib/api";
  import { colors, isActive, plural, relative, taskName } from "../lib/format";
  import { session } from "../lib/session.svelte";
  import CancelButton from "./CancelButton.svelte";
  import Dot from "./Dot.svelte";
  import StateBadge from "./StateBadge.svelte";

  let { onselect }: { onselect: (id: string) => void } = $props();

  const order: JobState[] = ["succeeded", "failed", "running", "collecting", "dispatching", "blocked", "queued", "cancelled", "skipped"];

  function layers(jobs: Job[]): Job[][] {
    const byId = new Map(jobs.map((j) => [j.id, j]));
    const depth = new Map<string, number>();
    const visit = (job: Job): number => {
      const known = depth.get(job.id);
      if (known !== undefined) return known;
      depth.set(job.id, 0);
      const level = Math.max(-1, ...(job.depends_on ?? []).flatMap((id) => byId.get(id) ?? []).map(visit)) + 1;
      depth.set(job.id, level);
      return level;
    };
    const result: Job[][] = [];
    for (const job of jobs) (result[visit(job)] ??= []).push(job);
    return result;
  }

  function overall(jobs: Job[]): JobState {
    if (jobs.some((j) => j.state === "failed")) return "failed";
    if (jobs.some((j) => j.state === "blocked")) return "blocked";
    if (jobs.some((j) => isActive(j.state))) return jobs.some((j) => j.state !== "queued") ? "running" : "queued";
    return jobs.every((j) => j.state === "succeeded") ? "succeeded" : "cancelled";
  }

  const workflows = $derived.by(() => {
    const groups = Map.groupBy(
      session.jobs.filter((j) => j.workflow),
      (j) => j.workflow!,
    );
    return [...groups]
      .map(([id, jobs]) => {
        const counts = Object.groupBy(jobs, (j) => j.state);
        let offset = 0;
        const segments = order.flatMap((state) => {
          const n = counts[state]?.length ?? 0;
          if (n === 0) return [];
          const segment = { state, x: (offset / jobs.length) * 100, width: (n / jobs.length) * 100, n };
          offset += n;
          return [segment];
        });
        return { id, jobs, layers: layers(jobs), segments, state: overall(jobs), done: counts.succeeded?.length ?? 0, created: jobs[0].created_at };
      })
      .toReversed();
  });
</script>

{#if workflows.length === 0}
  <div class="card px-6 py-16 text-center">
    <p class="font-medium">No workflows yet</p>
    <p class="mt-1 muted text-sm">Queue a workflow with ordo queue workflow to see its tasks grouped by dependency stage.</p>
  </div>
{:else}
  <div class="space-y-4">
    {#each workflows as flow (flow.id)}
      <section class="card p-5">
        <header class="flex flex-wrap items-start justify-between gap-3">
          <div class="min-w-0">
            <div class="flex items-center gap-2">
              <h2 class="truncate font-semibold">{flow.id}</h2>
              <StateBadge state={flow.state} />
            </div>
            <p class="mt-0.5 muted text-sm">{flow.done} of {flow.jobs.length} tasks succeeded · {plural(flow.layers.length, "stage")} · {relative(flow.created)}</p>
          </div>
          {#if flow.jobs.some((j) => isActive(j.state))}
            <CancelButton label="Cancel workflow" onconfirm={() => session.cancel("workflows", flow.id)} />
          {/if}
        </header>

        <svg class="mt-4 h-2 w-full overflow-hidden rounded-full" viewBox="0 0 100 2" preserveAspectRatio="none" role="img" aria-label={flow.segments.map((s) => `${s.n} ${s.state}`).join(", ")}>
          <rect width="100" height="2" class="fill-zinc-200 dark:fill-zinc-800" />
          {#each flow.segments as s (s.state)}<rect x={s.x} width={s.width} height="2" class={colors(s.state).dot} />{/each}
        </svg>

        <div class="mt-5 flex gap-3 overflow-x-auto pb-1">
          {#each flow.layers as layer, i (i)}
            <div class="w-56 shrink-0 space-y-1.5">
              <p class="label">Stage {i + 1}</p>
              {#each layer as job (job.id)}
                <button class="flex w-full items-center gap-2 rounded-lg border border-zinc-200 px-2.5 py-1.5 text-left text-sm hover:border-zinc-400 hover:bg-zinc-50 dark:border-zinc-800 dark:hover:border-zinc-600 dark:hover:bg-zinc-800/50" onclick={() => onselect(job.id)} title="{job.id}: {job.state}">
                  <Dot fill={colors(job.state).dot} class="size-2" />
                  <span class="truncate">{taskName(job)}</span>
                  <span class="sr-only">{job.state}</span>
                </button>
              {/each}
            </div>
          {/each}
        </div>
      </section>
    {/each}
  </div>
{/if}
