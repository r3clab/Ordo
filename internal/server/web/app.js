"use strict";
let token = "";
let refreshing = false;
const element = (id) => document.getElementById(id);
const status = (text) => { element("status").textContent = text; };
async function api(path, method = "GET", body) {
  if (!token) throw new Error("Connect with a token first.");
  const activeToken = token;
  const response = await fetch(`/v1/${path}`, { method, credentials: "omit", headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" }, body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await response.text();
  if (activeToken !== token) throw new Error("Session changed; response discarded.");
  if (!response.ok) throw new Error(`${response.status}: ${text}`);
  return response.headers.get("Content-Type")?.includes("application/json") ? JSON.parse(text) : text;
}
function button(parent, title, action) {
  const node = document.createElement("button"); node.textContent = title; node.type = "button";
  node.addEventListener("click", async () => { node.disabled = true; try { await action(); } catch (error) { status(error.message); } finally { node.disabled = false; } });
  parent.append(node);
}
async function refresh() {
  if (!token || refreshing) return;
  refreshing = true;
  try {
    const [jobs, workers] = await Promise.all([api("jobs"), api("workers")]);
    if (!token) return;
    element("workers").textContent = JSON.stringify(workers, null, 2);
    const body = element("jobs");
    if (body.contains(document.activeElement)) return;
    body.replaceChildren();
    for (const job of jobs) {
      const row = document.createElement("tr");
      const attempt = job.attempts?.at(-1);
      for (const text of [job.workflow ? `${job.id} / ${job.workflow}` : job.id, job.error ? `${job.state}: ${job.error}` : job.state, `${attempt?.worker ?? "—"} / ${job.attempts?.length ?? 0}`, JSON.stringify(job.spec.resources)]) {
        const cell = document.createElement("td"); cell.textContent = text; row.append(cell);
      }
      const actions = document.createElement("td"); const path = `jobs/${encodeURIComponent(job.id)}`;
      button(actions, "Details", () => { element("detail").textContent = JSON.stringify(job, null, 2); });
      for (const [index, run] of (job.attempts ?? []).entries()) {
        if (!run.result_dir) continue;
        for (const stream of ["stdout", "stderr"]) button(actions, `${index + 1}: ${stream}`, async () => { element("detail").textContent = await api(`${path}/logs/${stream}?attempt=${index}`); });
      }
      if (!["succeeded", "failed", "cancelled", "skipped"].includes(job.state)) {
        button(actions, "Cancel job", async () => { if (confirm(`Cancel ${job.id}?`)) { await api(`${path}/cancel`, "POST"); await refresh(); } });
        if (job.workflow) button(actions, "Cancel workflow", async () => { if (confirm(`Cancel all of ${job.workflow}?`)) { await api(`workflows/${encodeURIComponent(job.workflow)}/cancel`, "POST"); await refresh(); } });
      }
      row.append(actions); body.append(row);
    }
  } catch (error) { status(error.message); } finally { refreshing = false; }
}
element("login").addEventListener("submit", async (event) => { event.preventDefault(); token = element("token").value.trim(); element("token").value = ""; try { await api("workers"); status("Connected. Refreshing every two seconds; token is not persisted."); await refresh(); } catch (error) { token = ""; status(error.message); } });
element("disconnect").addEventListener("click", () => { token = ""; element("token").value = ""; element("jobs").replaceChildren(); element("workers").textContent = "Disconnected"; element("detail").textContent = ""; status("Disconnected; token cleared."); });
element("file").addEventListener("change", async () => { const file = element("file").files[0]; if (file) { if (file.size > 8 * 1024 * 1024) { status("Specification exceeds 8 MiB."); return; } element("spec").value = await file.text(); } });
element("submit").addEventListener("submit", async (event) => { event.preventDefault(); try { const result = await api(element("kind").value, "POST", JSON.parse(element("spec").value)); element("detail").textContent = JSON.stringify(result, null, 2); status("Submission accepted."); await refresh(); } catch (error) { status(error.message); } });
element("refresh").addEventListener("click", refresh);
setInterval(() => { if (element("automatic").checked) refresh(); }, 2000);
