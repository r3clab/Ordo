<script lang="ts" generics="T extends string">
  let { options, value, onchange, label }: { options: readonly (readonly [T, string])[]; value: T; onchange: (value: T) => void; label: string } = $props();
</script>

<div class="inline-flex rounded-lg bg-zinc-100 p-0.5 dark:bg-zinc-800" role="tablist" aria-label={label}>
  {#each options as [id, text] (id)}
    <button type="button" role="tab" aria-selected={value === id} class="rounded-md px-3 py-1 font-mono text-xs font-medium {value === id ? 'bg-white shadow-xs dark:bg-zinc-950' : 'muted hover:text-zinc-900 dark:hover:text-zinc-100'}" onclick={() => onchange(id)}>{text}</button>
  {/each}
</div>
