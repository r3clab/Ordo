<script lang="ts">
  let { fill, class: className = "size-1.5" }: { fill: string; class?: string } = $props();
</script>

<svg class="shrink-0 {className} {fill}" viewBox="0 0 6 6" aria-hidden="true"><circle cx="3" cy="3" r="3" /></svg>
