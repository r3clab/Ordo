<script lang="ts">
  import { message, request } from "../lib/api";
  import Segmented from "./Segmented.svelte";

  let { jobId, attempt }: { jobId: string; attempt: number } = $props();
  let stream = $state<"stdout" | "stderr">("stdout");
  let text = $state("");
  let truncated = $state(false);
  let loading = $state(false);
  let error = $state("");

  $effect(() => {
    let stale = false;
    const path = `jobs/${encodeURIComponent(jobId)}/logs/${stream}?attempt=${attempt}`;
    loading = true;
    error = "";
    request(path)
      .then(async (response) => {
        const body = await response.text();
        if (stale) return;
        text = body;
        truncated = response.headers.get("X-Ordo-Truncated") === "true";
      })
      .catch((e) => {
        if (stale) return;
        text = "";
        error = message(e);
      })
      .finally(() => {
        if (!stale) loading = false;
      });
    return () => (stale = true);
  });
</script>

<div class="space-y-2">
  <div class="flex items-center justify-between">
    <Segmented label="Log stream" options={[["stdout", "stdout"], ["stderr", "stderr"]] as const} value={stream} onchange={(s) => (stream = s)} />
    {#if truncated}<span class="text-xs text-amber-600 dark:text-amber-400">Showing the last 1 MiB</span>{/if}
  </div>
  {#if error}
    <p class="alert">{error}</p>
  {:else}
    <pre data-log class="max-h-96 min-h-24 overflow-auto rounded-lg bg-zinc-950 p-3 font-mono text-xs leading-relaxed whitespace-pre-wrap text-zinc-100 {loading ? 'opacity-60' : ''}">{text || (loading ? "Loading…" : "(empty)")}</pre>
  {/if}
</div>
