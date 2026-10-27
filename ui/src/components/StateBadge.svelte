<script lang="ts">
  import { colors } from "../lib/format";
  import Dot from "./Dot.svelte";

  let { state }: { state: string } = $props();
</script>

<span data-state={state} class="inline-flex shrink-0 items-center gap-1.5 rounded-full px-2 py-0.5 text-xs font-medium capitalize ring-1 ring-inset {colors(state).badge}">
  <Dot fill={colors(state).dot} />
  {state}
</span>
