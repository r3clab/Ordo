import { readFile, writeFile, mkdir, open } from "node:fs/promises";
import { execFileSync, spawn } from "node:child_process";
import { setTimeout as delay } from "node:timers/promises";
import assert from "node:assert/strict";
const [url, root, evidence] = process.argv.slice(2);
const profile = `${evidence}/browser`;
await mkdir(profile, { mode: 0o700 });
const log = await open(`${evidence}/browser.log`, "w", 0o600);
const browser = spawn(process.env.ORDO_TEST_CHROMIUM, ["--headless", "--no-sandbox", "--disable-gpu", "--disable-background-networking", "--remote-debugging-port=0", `--user-data-dir=${profile}`, "about:blank"], { stdio: ["ignore", log.fd, log.fd] });
let socket;
try {
  let port;
  for (let i = 0; i < 100; i++) { try { port = (await readFile(`${profile}/DevToolsActivePort`, "utf8")).split("\n")[0]; break; } catch { await delay(100); } }
  assert(port, "Chromium did not start");
  const pages = await (await fetch(`http://127.0.0.1:${port}/json`)).json();
  socket = new WebSocket(pages.find((page) => page.type === "page").webSocketDebuggerUrl);
  await new Promise((resolve, reject) => { socket.addEventListener("open", resolve, { once: true }); socket.addEventListener("error", reject, { once: true }); });
  let next = 0; const pending = new Map();
  const call = (method, params = {}) => new Promise((resolve, reject) => { const id = ++next; pending.set(id, { resolve, reject }); socket.send(JSON.stringify({ id, method, params })); });
  socket.addEventListener("message", (event) => { const message = JSON.parse(event.data); if (message.method === "Page.javascriptDialogOpening") { void call("Page.handleJavaScriptDialog", { accept: true }); } const waiter = pending.get(message.id); if (waiter) { pending.delete(message.id); if (message.error) waiter.reject(new Error(JSON.stringify(message.error))); else waiter.resolve(message.result); } });
  const evaluate = async (expression) => { const response = await call("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true }); if (response.exceptionDetails) throw new Error(JSON.stringify(response.exceptionDetails)); return response.result.value; };
  const wait = async (expression) => { for (let i = 0; i < 180; i++) { if (await evaluate(expression)) return; await delay(500); } throw new Error(`Timed out: ${expression}`); };
  await call("Page.enable"); await call("Page.navigate", { url });
  await wait('document.title === "Ordo" && !!document.querySelector("nav[aria-label=Views]")');
  const click = (selector, text) => `[...document.querySelectorAll(${JSON.stringify(selector)})].find(e=>e.textContent.trim()===${JSON.stringify(text)}).click()`;
  const row = (out) => `document.querySelector('tr[data-job-out=${JSON.stringify(out)}]')`;
  const state = (out, value) => `${row(out)}?.querySelector("[data-state]").dataset.state===${JSON.stringify(value)}`;
  assert(await evaluate('localStorage.length === 0 && sessionStorage.length === 0 && document.cookie === ""'));
  const image = process.env.ORDO_TEST_INSTALLABLE;
  const printer = `${evidence}/browser/log`, sleeper = `${evidence}/browser/stop`;
  const jobs = [
    { out: printer, image, resources: { cores: 1, memory_mib: 128 }, args: ["-c", "printf '<script>globalThis.injection=true</script>\\n'"] },
    { out: sleeper, image, resources: { cores: 1, memory_mib: 128 }, timeout_seconds: 60, args: ["-c", "sleep 45"] },
  ];
  const file = `${evidence}/browser-jobs.jsonl`;
  await writeFile(file, jobs.map((job) => JSON.stringify(job)).join("\n"), { mode: 0o600 });
  execFileSync(process.env.ORDO_TEST_BINARY, ["submit", "--state", root, "--file", file], { stdio: "ignore" });
  await evaluate('document.querySelector(\'a[href="#/jobs"]\').click()');
  await wait(state(sleeper, "running"));
  await evaluate(`${row(sleeper)}.click()`);
  await evaluate(click("aside button", "Cancel job"));
  await evaluate(click("aside button", "Yes, cancel"));
  await wait(state(sleeper, "cancelled"));
  await evaluate(`document.querySelector('[aria-label="Close details"]').click()`);
  await wait(state(printer, "succeeded"));
  await evaluate(`${row(printer)}.click()`);
  await wait('document.querySelector("aside [data-log]")?.textContent.includes("<script>")');
  assert(await evaluate('globalThis.injection === undefined && document.querySelector("aside script") === null'));
  const screenshot = await call("Page.captureScreenshot"); await writeFile(`${evidence}/web.png`, Buffer.from(screenshot.data, "base64"));
  console.log("PASS: real browser queued job status, running cancellation, safe logs");
} finally { socket?.close(); browser.kill("SIGTERM"); await new Promise((resolve) => { if (browser.exitCode !== null) resolve(); else browser.once("exit", resolve); }); await log.close(); }
