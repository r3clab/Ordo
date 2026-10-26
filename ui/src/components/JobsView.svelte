<script lang="ts">
  import { filters, relative, resources, shortImage, taskName } from "../lib/format";
  import { session } from "../lib/session.svelte";
  import Icon from "./Icon.svelte";
  import StateBadge from "./StateBadge.svelte";

  let { onselect }: { onselect: (id: string) => void } = $props();
  let filter = $state.raw(filters[0]);
  let query = $state("");

  const visible = $derived.by(() => {
    const needle = query.trim().toLowerCase();
    return session.jobs
      .filter(filter.match)
      .filter((j) => !needle || [j.id, j.workflow ?? "", j.spec.image, j.attempts?.at(-1)?.worker ?? ""].some((field) => field.toLowerCase().includes(needle)))
      .toReversed();
  });
</script>

<div class="space-y-4">
  <div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
    <div class="flex flex-wrap gap-1.5" role="tablist" aria-label="Filter jobs by state">
      {#each filters as f (f.id)}
        <button role="tab" aria-selected={filter === f} onclick={() => (filter = f)} class="btn rounded-full px-3 py-1 {filter === f ? 'bg-zinc-900 text-white dark:bg-zinc-100 dark:text-zinc-900' : 'bg-white text-zinc-600 ring-1 ring-zinc-200 hover:bg-zinc-100 dark:bg-zinc-900 dark:text-zinc-300 dark:ring-zinc-800 dark:hover:bg-zinc-800'}">
          {f.label}
          <span class="tabular-nums opacity-60">{session.jobs.filter(f.match).length}</span>
        </button>
      {/each}
    </div>
    <label class="relative block sm:w-72">
      <span class="sr-only">Search jobs</span>
      <Icon name="search" class="pointer-events-none absolute top-2.5 left-3 size-4 text-zinc-400" />
      <input class="input pl-9" type="search" bind:value={query} placeholder="Search ID, workflow, image, worker" />
    </label>
  </div>

  <div class="card overflow-hidden">
    {#if visible.length === 0}
      <div class="px-6 py-16 text-center">
        <p class="font-medium">{session.jobs.length === 0 ? "No jobs yet" : "No matching jobs"}</p>
        <p class="muted mt-1 text-sm">{session.jobs.length === 0 ? "Submit a job or workflow to get started." : "Try another filter or search."}</p>
      </div>
    {:else}
      <div class="overflow-x-auto">
        <table class="w-full text-left text-sm">
          <thead class="muted border-b border-zinc-200 bg-zinc-50 text-xs dark:border-zinc-800 dark:bg-zinc-900/50">
            <tr>
              <th class="px-4 py-2.5 font-medium">Job</th>
              <th class="px-4 py-2.5 font-medium">State</th>
              <th class="hidden px-4 py-2.5 font-medium md:table-cell">Worker</th>
              <th class="hidden px-4 py-2.5 font-medium lg:table-cell">Resources</th>
              <th class="hidden px-4 py-2.5 font-medium sm:table-cell">Created</th>
            </tr>
          </thead>
          <tbody class="divide-y divide-zinc-100 dark:divide-zinc-800">
            {#each visible as job (job.id)}
              {@const attempt = job.attempts?.at(-1)}
              <tr data-job-id={job.id} class="cursor-pointer hover:bg-zinc-50 dark:hover:bg-zinc-800/50" onclick={() => onselect(job.id)}>
                <td class="max-w-80 px-4 py-3">
                  <button class="block max-w-full truncate text-left font-medium hover:underline">{taskName(job)}</button>
                  <div class="muted truncate text-xs">
                    {#if job.workflow}<span class="font-medium text-zinc-600 dark:text-zinc-300">{job.workflow}</span> · {/if}<span class="font-mono">{shortImage(job.spec.image)}</span>
                  </div>
                </td>
                <td class="px-4 py-3">
                  <StateBadge state={job.state} />
                  {#if job.error}<p class="mt-1 max-w-64 truncate text-xs text-rose-600 dark:text-rose-400" title={job.error}>{job.error}</p>{/if}
                </td>
                <td class="hidden px-4 py-3 md:table-cell">
                  {#if attempt}{attempt.worker}<span class="text-zinc-400"> · attempt {job.attempts?.length}</span>{:else}<span class="text-zinc-400">—</span>{/if}
                </td>
                <td class="hidden px-4 py-3 text-zinc-600 lg:table-cell dark:text-zinc-300">{resources(job)}</td>
                <td class="muted hidden px-4 py-3 whitespace-nowrap sm:table-cell" title={new Date(job.created_at).toLocaleString()}>{relative(job.created_at)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    {/if}
  </div>
</div>
