<script lang="ts" generics="T extends string">
  let { options, value, onchange, label, role = "tablist", class: className = "px-3 py-1 text-xs" }: { options: readonly (readonly [T, string])[]; value: T; onchange: (value: T) => void; label: string; role?: "tablist" | "radiogroup"; class?: string } = $props();
  const item = $derived(role === "tablist" ? "tab" : "radio");
</script>

<div class="inline-flex rounded-lg bg-zinc-100 p-0.5 dark:bg-zinc-800" {role} aria-label={label}>
  {#each options as [id, text] (id)}
    <button type="button" role={item} aria-selected={item === "tab" ? value === id : undefined} aria-checked={item === "radio" ? value === id : undefined} class="rounded-md font-medium {className} {value === id ? 'bg-white shadow-xs dark:bg-zinc-950' : 'muted hover:text-zinc-900 dark:hover:text-zinc-100'}" onclick={() => onchange(id)}>{text}</button>
  {/each}
</div>
