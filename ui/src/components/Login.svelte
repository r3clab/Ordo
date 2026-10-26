<script lang="ts">
  import { message } from "../lib/api";
  import { session } from "../lib/session.svelte";
  import Logo from "./Logo.svelte";

  let token = $state("");
  let busy = $state(false);
  let error = $state("");

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    busy = true;
    error = "";
    try {
      await session.connect(token.trim());
    } catch (e) {
      error = message(e);
    } finally {
      busy = false;
      token = "";
    }
  }
</script>

<main class="grid min-h-dvh place-items-center p-4">
  <form id="login" onsubmit={submit} class="card w-full max-w-sm space-y-6 p-8">
    <div class="space-y-1">
      <div class="flex items-center gap-2">
        <Logo class="size-8 text-sm" />
        <h1 class="text-xl font-semibold tracking-tight">Ordo</h1>
      </div>
      <p class="muted text-sm">Reproducible evaluations across SSH workers.</p>
    </div>
    <div class="space-y-1.5">
      <label for="token" class="text-sm font-medium">Access token</label>
      <input id="token" class="input font-mono" type="password" bind:value={token} required autocomplete="off" spellcheck="false" placeholder="64-character hex token" aria-describedby="token-help" />
      <p id="token-help" class="muted text-xs">Read it from the <code class="font-mono">token</code> file in the server state directory. It stays in this tab's memory only.</p>
    </div>
    {#if error || session.error}
      <p role="alert" class="alert">{error || session.error}</p>
    {/if}
    <button class="btn-primary w-full py-2" disabled={busy || token.trim() === ""}>{busy ? "Connecting…" : "Connect"}</button>
  </form>
</main>
