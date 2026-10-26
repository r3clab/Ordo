<script lang="ts">
  import type { Job } from "../lib/api";
  import { message } from "../lib/api";
  import { session } from "../lib/session.svelte";
  import Icon from "./Icon.svelte";
  import Segmented from "./Segmented.svelte";

  let { open = $bindable(false), onsubmitted }: { open: boolean; onsubmitted: (jobs: Job[], workflow: boolean) => void } = $props();

  const templates = {
    jobs: JSON.stringify({ id: "example", image: { archive: "/absolute/path/image.tar", id: "sha256:IMAGE_ID" }, resources: { cores: 1, memory_mib: 128 }, args: ["-c", "printf hello > /outputs/message"] }, null, 2),
    workflows: JSON.stringify(
      {
        id: "example-flow",
        image: { archive: "/absolute/path/image.tar", id: "sha256:IMAGE_ID" },
        tasks: [
          { id: "prepare", resources: { cores: 1, memory_mib: 128 }, args: ["-c", "echo data > /outputs/data"] },
          { id: "evaluate", depends_on: ["prepare"], from: { data: "prepare" }, matrix: { seed: ["1", "2"] }, resources: { cores: 1, memory_mib: 128 }, args: ["-c", "cat /inputs/data/data; echo seed {{seed}}"] },
        ],
      },
      null,
      2,
    ),
  };
  type Kind = keyof typeof templates;

  let dialog: HTMLDialogElement;
  let kind = $state<Kind>("jobs");
  let text = $state(templates.jobs);
  let busy = $state(false);
  let error = $state("");

  const json = $derived.by((): { value?: any; error?: string } => {
    try {
      return { value: JSON.parse(text) };
    } catch (e) {
      return { error: message(e) };
    }
  });
  const problem = $derived.by(() => {
    const value = json.value;
    if (json.error) return json.error;
    if (typeof value !== "object" || value === null || Array.isArray(value)) return "The specification must be a JSON object.";
    if (kind === "workflows" && !Array.isArray(value.tasks)) return 'A workflow needs a "tasks" array.';
    if (kind === "jobs" && typeof value.resources !== "object") return 'A job needs "resources" with cores and memory_mib.';
    return "";
  });

  $effect(() => {
    if (open && !dialog.open) dialog.showModal();
    else if (!open && dialog.open) dialog.close();
  });

  function choose(next: Kind) {
    if (text === templates[kind]) text = templates[next];
    kind = next;
  }

  async function load(event: Event & { currentTarget: HTMLInputElement }) {
    const file = event.currentTarget.files?.[0];
    event.currentTarget.value = "";
    if (!file) return;
    if (file.size > 8 << 20) {
      error = "Specifications are limited to 8 MiB.";
      return;
    }
    text = await file.text();
    kind = Array.isArray(json.value?.tasks) ? "workflows" : "jobs";
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (problem) return;
    busy = true;
    error = "";
    try {
      const result = await (await session.call(kind, "POST", json.value)).json();
      const jobs: Job[] = Array.isArray(result) ? result : [result];
      open = false;
      session.flash(kind === "workflows" ? `Workflow submitted with ${jobs.length} tasks.` : `Job ${jobs[0]?.id} submitted.`);
      await session.refresh();
      onsubmitted(jobs, kind === "workflows");
    } catch (e) {
      error = message(e);
    } finally {
      busy = false;
    }
  }
</script>

<dialog bind:this={dialog} onclose={() => (open = false)} class="m-auto w-[min(100%-2rem,48rem)] rounded-2xl border border-zinc-200 bg-white p-0 text-zinc-900 shadow-2xl backdrop:bg-zinc-950/40 backdrop:backdrop-blur-[1px] dark:border-zinc-800 dark:bg-zinc-900 dark:text-zinc-100">
  <form id="submit" onsubmit={submit} class="flex max-h-[90dvh] flex-col">
    <header class="flex items-center justify-between border-b border-zinc-200 px-6 py-4 dark:border-zinc-800">
      <h2 class="text-lg font-semibold">New submission</h2>
      <button type="button" class="btn-ghost p-1.5" onclick={() => (open = false)} aria-label="Close"><Icon name="x" class="size-5" /></button>
    </header>
    <div class="space-y-4 overflow-y-auto px-6 py-5">
      <div class="flex flex-wrap items-center justify-between gap-3">
        <Segmented label="Submission type" role="radiogroup" options={[["jobs", "Job"], ["workflows", "Workflow"]] as const} value={kind} onchange={choose} class="px-4 py-1.5 text-sm" />
        <div class="flex gap-2">
          <button type="button" class="btn-ghost" onclick={() => (text = templates[kind])}>Use template</button>
          <label class="btn-secondary cursor-pointer">
            <Icon name="upload" />
            Load file
            <input type="file" accept="application/json,.json" class="sr-only" onchange={load} />
          </label>
        </div>
      </div>
      <label class="block">
        <span class="sr-only">Specification JSON</span>
        <textarea id="spec" bind:value={text} spellcheck="false" rows="16" class="input resize-y font-mono text-xs leading-relaxed"></textarea>
      </label>
      <p class="text-xs {problem ? 'text-rose-600 dark:text-rose-400' : 'text-emerald-600 dark:text-emerald-400'}">{problem || "Valid JSON"}</p>
      <p class="muted text-xs">Archive and input paths refer to files on the server, not this browser. A plain image reference is exported from the server's local Podman.</p>
      {#if error}<p role="alert" class="alert">{error}</p>{/if}
    </div>
    <footer class="flex justify-end gap-2 border-t border-zinc-200 px-6 py-4 dark:border-zinc-800">
      <button type="button" class="btn-secondary" onclick={() => (open = false)}>Cancel</button>
      <button class="btn-primary" disabled={busy || problem !== ""}>{busy ? "Submitting…" : "Submit"}</button>
    </footer>
  </form>
</dialog>
