<script lang="ts">
  import { message } from "../lib/api";
  import { session } from "../lib/session.svelte";

  let { label, onconfirm }: { label: string; onconfirm: () => Promise<void> } = $props();
  let confirming = $state(false);
  let busy = $state(false);

  async function confirm() {
    busy = true;
    try {
      await onconfirm();
    } catch (error) {
      session.error = message(error);
    } finally {
      busy = false;
      confirming = false;
    }
  }
</script>

{#if confirming}
  <span class="inline-flex items-center gap-1.5">
    <span class="text-sm text-zinc-500 dark:text-zinc-400">Are you sure?</span>
    <button class="btn-danger" disabled={busy} onclick={confirm}>{busy ? "Cancelling…" : "Yes, cancel"}</button>
    <button class="btn-ghost" disabled={busy} onclick={() => (confirming = false)}>Keep</button>
  </span>
{:else}
  <button class="btn-secondary text-rose-600 dark:text-rose-400" onclick={() => (confirming = true)}>{label}</button>
{/if}
