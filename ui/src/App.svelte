<script lang="ts">
  import type { Job } from "./lib/api";
  import { relative } from "./lib/format";
  import { session } from "./lib/session.svelte";
  import Icon from "./components/Icon.svelte";
  import JobDetail from "./components/JobDetail.svelte";
  import Logo from "./components/Logo.svelte";
  import JobsView from "./components/JobsView.svelte";
  import Login from "./components/Login.svelte";
  import SubmitDialog from "./components/SubmitDialog.svelte";
  import WorkersView from "./components/WorkersView.svelte";
  import WorkflowsView from "./components/WorkflowsView.svelte";

  const views = [
    { id: "jobs", label: "Jobs", icon: "list" },
    { id: "workflows", label: "Workflows", icon: "layers" },
    { id: "workers", label: "Workers", icon: "server" },
  ] as const;
  type View = (typeof views)[number]["id"];

  const readView = (): View => views.find((v) => location.hash === `#/${v.id}`)?.id ?? "jobs";
  let view = $state<View>(readView());
  let selected = $state<string | null>(null);
  let submitting = $state(false);
  const job = $derived(session.jobs.find((j) => j.id === selected));
  const online = $derived(session.workers.filter((w) => w.connected).length);
  const select = (id: string | null) => (selected = id);

  $effect(() => {
    if (!session.connected) {
      selected = null;
      submitting = false;
    }
  });

  $effect(() => {
    if (location.hash !== `#/${view}`) history.replaceState(null, "", `#/${view}`);
  });

  function submitted(jobs: Job[], workflow: boolean) {
    if (workflow) view = "workflows";
    else selected = jobs[0]?.id ?? null;
  }
</script>

<svelte:window onhashchange={() => (view = readView())} />

{#if !session.connected}
  <Login />
{:else}
  <div class="min-h-dvh">
    <header class="sticky top-0 z-30 border-b border-zinc-200 bg-white/80 backdrop-blur dark:border-zinc-800 dark:bg-zinc-950/80">
      <div class="mx-auto flex max-w-7xl items-center gap-4 px-4 py-3 sm:px-6">
        <div class="flex items-center gap-2">
          <Logo />
          <span class="hidden font-semibold tracking-tight sm:inline">Ordo</span>
        </div>
        <nav class="flex gap-1" aria-label="Views">
          {#each views as v (v.id)}
            <a href="#/{v.id}" aria-current={view === v.id ? "page" : undefined} class="btn {view === v.id ? 'bg-zinc-100 text-zinc-900 dark:bg-zinc-800 dark:text-zinc-100' : 'text-zinc-500 hover:text-zinc-900 dark:text-zinc-400 dark:hover:text-zinc-100'}">
              <Icon name={v.icon} />
              <span class="hidden sm:inline">{v.label}</span>
            </a>
          {/each}
        </nav>
        <div class="ml-auto flex items-center gap-1.5">
          <button class="btn-ghost hidden md:inline-flex" onclick={() => (session.live = !session.live)} aria-pressed={session.live} title={session.live ? "Pause auto-refresh" : "Resume auto-refresh"}>
            <span class="relative flex size-2">
              {#if session.live}<span class="absolute inline-flex size-full animate-ping rounded-full bg-emerald-400 opacity-75"></span>{/if}
              <span class="relative inline-flex size-2 rounded-full {session.live ? 'bg-emerald-500' : 'bg-zinc-400'}"></span>
            </span>
            <span class="text-xs tabular-nums">{session.live ? `Live · ${relative(session.updated)}` : "Paused"}</span>
          </button>
          <button class="btn-ghost p-1.5" onclick={() => session.refresh()} aria-label="Refresh now" title="Refresh now"><Icon name="refresh" /></button>
          <button class="btn-primary" onclick={() => (submitting = true)}><Icon name="plus" /><span class="hidden sm:inline">Submit</span></button>
          <button class="btn-ghost p-1.5" onclick={() => session.disconnect()} aria-label="Disconnect" title="Disconnect"><Icon name="logout" /></button>
        </div>
      </div>
    </header>

    <main class="mx-auto max-w-7xl space-y-4 px-4 py-6 sm:px-6">
      <div class="flex items-baseline justify-between gap-4">
        <h1 class="text-2xl font-semibold tracking-tight">{views.find((v) => v.id === view)?.label}</h1>
        <p class="muted text-sm">{session.jobs.length} jobs · {online} of {session.workers.length} workers online</p>
      </div>
      {#if session.error}
        <div role="alert" class="alert flex items-start justify-between gap-3">
          <span>{session.error}</span>
          <button class="shrink-0" onclick={() => (session.error = "")} aria-label="Dismiss"><Icon name="x" /></button>
        </div>
      {/if}
      {#if view === "jobs"}
        <JobsView onselect={select} />
      {:else if view === "workflows"}
        <WorkflowsView onselect={select} />
      {:else}
        <WorkersView />
      {/if}
    </main>
  </div>

  {#if job}
    {#key job.id}
      <JobDetail {job} onclose={() => select(null)} onselect={select} />
    {/key}
  {/if}
  <SubmitDialog bind:open={submitting} onsubmitted={submitted} />

  <div class="pointer-events-none fixed inset-x-0 bottom-4 z-50 flex justify-center px-4" aria-live="polite">
    {#if session.notice}
      <p class="pointer-events-auto rounded-lg bg-zinc-900 px-4 py-2 text-sm text-white shadow-lg dark:bg-zinc-100 dark:text-zinc-900">{session.notice}</p>
    {/if}
  </div>
{/if}
