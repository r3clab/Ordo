import { readFile, writeFile, mkdir, open } from "node:fs/promises";
import { spawn } from "node:child_process";
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
  await wait('document.title === "Ordo" && !!document.getElementById("token")');
  assert.equal((await fetch(`${url}/v1/jobs`)).status, 401);
  const token = (await readFile(`${root}/token`, "utf8")).trim();
  const input = (selector, value) => `(()=>{const e=document.querySelector(${JSON.stringify(selector)});e.value=${JSON.stringify(value)};e.dispatchEvent(new Event("input",{bubbles:true}))})()`;
  const click = (selector, text) => `[...document.querySelectorAll(${JSON.stringify(selector)})].find(e=>e.textContent.trim()===${JSON.stringify(text)}).click()`;
  const row = (id) => `document.querySelector('tr[data-job-id=${JSON.stringify(id)}]')`;
  const state = (id, value) => `${row(id)}?.querySelector("[data-state]").dataset.state===${JSON.stringify(value)}`;
  await evaluate(input("#token", token));
  await evaluate('document.getElementById("login").requestSubmit()');
  await wait('!!document.querySelector("nav[aria-label=Views]")');
  assert(await evaluate('localStorage.length === 0 && sessionStorage.length === 0 && document.cookie === ""'));
  const workflow = { id: "browser", image: process.env.ORDO_TEST_IMAGE, archive: process.env.ORDO_TEST_ARCHIVE, tasks: [
    { id: "log", resources: { cores: 1, memory_mib: 128 }, args: ["-c", "printf '<script>globalThis.injection=true</script>\\n'"] },
    { id: "stop", resources: { cores: 1, memory_mib: 128 }, timeout_seconds: 60, args: ["-c", "sleep 45"] },
  ] };
  await evaluate(click("header button", "Submit"));
  await wait('document.querySelector("dialog").open');
  await evaluate(click("[role=radio]", "Workflow"));
  await evaluate(input("#spec", JSON.stringify(workflow)));
  await evaluate('document.getElementById("submit").requestSubmit()');
  await wait('document.body.textContent.includes("Workflow submitted with 2 tasks.")');
  await evaluate('document.querySelector(\'a[href="#/jobs"]\').click()');
  await wait(state("browser--stop", "running"));
  await evaluate(`${row("browser--stop")}.click()`);
  await evaluate(click("aside button", "Cancel job"));
  await evaluate(click("aside button", "Yes, cancel"));
  await wait(state("browser--stop", "cancelled"));
  await evaluate(`document.querySelector('[aria-label="Close details"]').click()`);
  await wait(state("browser--log", "succeeded"));
  await evaluate(`${row("browser--log")}.click()`);
  await wait('document.querySelector("aside [data-log]")?.textContent.includes("<script>")');
  assert(await evaluate('globalThis.injection === undefined && document.querySelector("aside script") === null'));
  const screenshot = await call("Page.captureScreenshot"); await writeFile(`${evidence}/web.png`, Buffer.from(screenshot.data, "base64"));
  await evaluate(`document.querySelector('[aria-label="Disconnect"]').click()`);
  await wait('!!document.getElementById("token")');
  assert(await evaluate('document.getElementById("token").value === "" && document.querySelector("tr[data-job-id]") === null && localStorage.length === 0 && sessionStorage.length === 0'));
  console.log("PASS: real browser authentication, workflow submission, status, running cancellation, safe logs, disconnect");
} finally { socket?.close(); browser.kill("SIGTERM"); await new Promise((resolve) => { if (browser.exitCode !== null) resolve(); else browser.once("exit", resolve); }); await log.close(); }
