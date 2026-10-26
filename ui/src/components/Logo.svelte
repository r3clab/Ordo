<script lang="ts">
  let { class: className = "size-7 text-xs" }: { class?: string } = $props();
</script>

<span class="grid place-items-center rounded-lg bg-zinc-900 font-bold text-white dark:bg-zinc-100 dark:text-zinc-900 {className}" aria-hidden="true">O</span>
