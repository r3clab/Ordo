<script lang="ts">
  import type { Job } from "../lib/api";
  import { duration, isActive, relative, resources, shortImage, shortOut } from "../lib/format";
  import { session } from "../lib/session.svelte";
  import CancelButton from "./CancelButton.svelte";
  import Icon from "./Icon.svelte";
  import LogViewer from "./LogViewer.svelte";
  import StateBadge from "./StateBadge.svelte";

  let { job, onclose, onselect }: { job: Job; onclose: () => void; onselect: (id: string) => void } = $props();

  const attempts = $derived(job.attempts ?? []);
  const multistep = $derived(job.steps.length > 1);
  const spec = $derived(job.steps[Math.min(job.step, job.steps.length - 1)].spec);
  const collected = $derived(attempts.flatMap((a, i) => (a.result_dir ? [i] : [])));
  let chosen = $state<number | null>(null);
  const logAttempt = $derived(chosen !== null && collected.includes(chosen) ? chosen : collected.at(-1));
  const facts = $derived(
    [
      ["Image", spec.image],
      ["Resources", resources(spec.resources)],
      ["Timeout", spec.timeout_seconds ? `${spec.timeout_seconds}s` : "None"],
      ["Retries", `${job.retry?.task ?? 0} task · ${job.retry?.infrastructure ?? 1} infrastructure`],
      ["Created", new Date(job.created_at).toLocaleString()],
      ["Worker tags", spec.tags?.join(", ") ?? ""],
      ["Arguments", spec.args?.length ? spec.args.join(" ") : ""],
      ["Environment", Object.keys(spec.env ?? {}).join(", ")],
      ["Inputs", Object.entries(job.inputs ?? {}).map(([name, source]) => `${name} ← ${source}`).join("\n")],
    ].filter(([, value]) => value),
  );
</script>

<svelte:window onkeydown={(e) => e.key === "Escape" && onclose()} />

<div class="fixed inset-0 z-40 bg-zinc-950/30 backdrop-blur-[1px]" role="presentation" onclick={onclose}></div>
<aside class="fixed inset-y-0 right-0 z-50 flex w-full max-w-2xl flex-col border-l border-zinc-200 bg-white shadow-2xl dark:border-zinc-800 dark:bg-zinc-900" aria-label="Job details">
  <header class="flex items-start justify-between gap-4 border-b border-zinc-200 px-6 py-4 dark:border-zinc-800">
    <div class="min-w-0 space-y-1">
      <h2 class="text-lg font-semibold break-all">{job.out}</h2>
      <p class="muted truncate font-mono text-xs" title="Job ID">{job.id}</p>
      <div class="flex flex-wrap items-center gap-2">
        <StateBadge state={job.state} />
        {#if job.cancel_requested && isActive(job.state)}<span class="text-xs text-amber-600 dark:text-amber-400">Cancellation requested</span>{/if}
      </div>
    </div>
    <button class="btn-ghost p-1.5" onclick={onclose} aria-label="Close details"><Icon name="x" class="size-5" /></button>
  </header>

  <div class="flex-1 space-y-6 overflow-y-auto px-6 py-5">
    {#if job.error}
      <p class="alert">{job.error}</p>
    {/if}

    {#if isActive(job.state)}
      <div class="flex flex-wrap gap-2">
        <CancelButton label="Cancel job" onconfirm={() => session.cancel(job.id)} />
      </div>
    {/if}

    <section>
      <h3 class="label mb-2">Overview</h3>
      <dl class="grid grid-cols-[8rem_1fr] gap-x-4 gap-y-2 text-sm">
        {#each facts as [term, value] (term)}
          <dt class="muted">{term}</dt>
          <dd class="font-mono text-xs leading-5 break-all whitespace-pre-line">{value}</dd>
        {/each}
      </dl>
    </section>

    {#if multistep}
      <section>
        <h3 class="label mb-2">Steps</h3>
        <ol class="space-y-1 text-sm">
          {#each job.steps as stage, i (i)}
            <li class="flex flex-wrap items-baseline gap-x-2 rounded-lg px-2 py-1 {i === job.step ? 'bg-zinc-100 dark:bg-zinc-800' : ''}" aria-current={i === job.step ? "step" : undefined}>
              <span class="font-medium">{stage.name ?? `Step ${i + 1}`}</span>
              <span class="muted min-w-0 truncate font-mono text-xs" title={stage.spec.image}>{shortImage(stage.spec.image)}</span>
              <span class="muted ml-auto text-xs">{resources(stage.spec.resources)}</span>
            </li>
          {/each}
        </ol>
      </section>
    {/if}

    {#if job.after?.length}
      <section>
        <h3 class="label mb-2">Waits for</h3>
        {#if job.waiting?.length}<p class="muted mb-2 text-xs">Inputs not yet available: {job.waiting.join(", ")}</p>{/if}
        <div class="flex flex-wrap gap-1.5">
          {#each job.after as id (id)}
            {@const dep = session.jobs.find((j) => j.id === id)}
            <button class="btn-secondary max-w-full py-1 text-xs" title={dep?.out ?? id} onclick={() => onselect(id)}>
              <span class="truncate">{dep ? shortOut(dep.out) : id}</span>
              {#if dep}<span class="text-zinc-400">· {dep.state}</span>{/if}
            </button>
          {/each}
        </div>
      </section>
    {/if}

    <section>
      <h3 class="label mb-2">Attempts</h3>
      {#if attempts.length === 0}
        <p class="text-sm muted">Not dispatched yet.</p>
      {:else}
        <ol class="space-y-2">
          {#each attempts as attempt, i (attempt.id)}
            {@const result = attempt.result}
            <li class="rounded-lg border border-zinc-200 p-3 text-sm dark:border-zinc-800">
              <div class="flex flex-wrap items-center justify-between gap-2">
                <span class="font-medium">#{i + 1}{multistep ? ` · step ${attempt.step + 1}` : ""} on {attempt.worker}</span>
              </div>
              <p class="mt-1 text-xs muted">
                {#if result?.exit_code !== undefined}Exit {result.exit_code}{result.oom_killed ? " (out of memory)" : ""} · {duration(result.started_at, result.finished_at)}{:else}Dispatched {relative(attempt.created_at)}{/if}{#if attempt.failure}
                  · {attempt.failure} failure{/if}{#if attempt.stop_requested}
                  · stop: {attempt.stop_requested}{attempt.stop_reason ? ` (${attempt.stop_reason})` : ""}{/if}
              </p>
            </li>
          {/each}
        </ol>
      {/if}
    </section>

    <section>
      <div class="mb-2 flex items-center justify-between">
        <h3 class="label">Logs</h3>
        {#if collected.length > 1}
          <select class="input w-auto py-1 text-xs" value={logAttempt} onchange={(e) => (chosen = Number(e.currentTarget.value))} aria-label="Attempt">
            {#each collected as i (i)}<option value={i}>Attempt {i + 1}{multistep ? ` · step ${attempts[i].step + 1}` : ""}</option>{/each}
          </select>
        {/if}
      </div>
      {#if logAttempt === undefined}
        <p class="text-sm muted">Logs appear once an attempt's results are collected from its worker.</p>
      {:else}
        <LogViewer jobId={job.id} attempt={logAttempt} />
      {/if}
    </section>

    <details class="group">
      <summary class="label cursor-pointer select-none">Raw JSON</summary>
      <pre class="mt-2 max-h-96 overflow-auto rounded-lg bg-zinc-100 p-3 font-mono text-xs dark:bg-zinc-950">{JSON.stringify(job, null, 2)}</pre>
    </details>
  </div>
</aside>
