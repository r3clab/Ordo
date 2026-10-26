<script lang="ts">
  let { label, value, max, detail }: { label: string; value: number; max: number; detail: string } = $props();
  const percent = $derived(max > 0 ? Math.min(100, (value / max) * 100) : 0);
</script>

<div class="space-y-1">
  <div class="flex justify-between text-xs">
    <span class="muted">{label}</span>
    <span class="font-medium tabular-nums">{detail}</span>
  </div>
  <svg class="h-1.5 w-full overflow-hidden rounded-full" viewBox="0 0 100 2" preserveAspectRatio="none" role="img" aria-label="{label}: {detail}">
    <rect width="100" height="2" class="fill-zinc-200 dark:fill-zinc-800" />
    <rect width={percent} height="2" class={percent >= 90 ? "fill-amber-500" : "fill-sky-500"} />
  </svg>
</div>
