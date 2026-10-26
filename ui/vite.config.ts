import { svelte } from "@sveltejs/vite-plugin-svelte";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vite";

const server = "http://127.0.0.1:7331";

export default defineConfig({
  plugins: [svelte(), tailwindcss()],
  build: {
    outDir: "../internal/server/web",
    emptyOutDir: true,
    cssCodeSplit: false,
    modulePreload: { polyfill: false },
    rollupOptions: {
      output: {
        // The Go server serves exactly index.html, app.js and style.css.
        entryFileNames: "app.js",
        assetFileNames: "style.css",
      },
    },
  },
  server: {
    proxy: {
      "/v1": {
        target: server,
        changeOrigin: true,
        // The API rejects cross-origin writes, so present the proxied request as same-origin.
        headers: { origin: server },
      },
    },
  },
});
