import assert from "node:assert/strict";
import test from "node:test";
import { createServer } from "vite";

// Use the same Svelte/TypeScript transforms as the app, without a browser or live workers.
test("job polling keeps unchanged data and only remembers successfully applied ETags", async (t) => {
  const server = await createServer({ server: { middlewareMode: true, watch: null }, appType: "custom" });
  t.after(() => server.close());
  const { session } = await server.ssrLoadModule("/src/lib/session.svelte.ts");
  let expectedTag;
  let jobs = [{ id: "job", state: "queued" }];
  let tag = '"one"';
  let status = 200;
  let workersStatus = 200;
  let malformed = false;
  let workerRequests = 0;
  let detailRequests = 0;
  t.mock.method(globalThis, "fetch", async (url, options) => {
    if (url === "/v1/jobs") {
      assert.equal(new Headers(options.headers).get("If-None-Match") ?? undefined, expectedTag);
      const body = status === 304 ? null : malformed ? "invalid JSON" : JSON.stringify(jobs);
      return new Response(body, { status, headers: { ETag: tag } });
    }
    if (url === "/v1/workers") {
      workerRequests++;
      return new Response(JSON.stringify([{ host: "worker" }]), { status: workersStatus });
    }
    assert.equal(url, "/v1/jobs/job");
    detailRequests++;
    return Response.json({ id: "job", state: "running" });
  });

  await session.refresh();
  assert.deepEqual(session.jobs, jobs);
  assert.equal(session.error, "");
  const first = session.jobs;
  expectedTag = tag;
  status = 304;
  session.selected = "job";
  await session.refresh();
  assert.equal(session.jobs, first);
  assert.equal(session.error, "");
  assert.equal(workerRequests, 2);
  assert.equal(detailRequests, 1);
  assert.equal(session.detail.state, "running");

  status = 200;
  tag = '"two"';
  jobs = [{ id: "job", state: "running" }];
  await session.refresh();
  assert.deepEqual(session.jobs, jobs);
  assert.notEqual(session.jobs, first);
  expectedTag = tag;
  const second = session.jobs;

  for (const failure of ["jobs", "workers", "json"]) {
    status = failure === "jobs" ? 500 : 200;
    workersStatus = failure === "workers" ? 500 : 200;
    malformed = failure === "json";
    tag = '"three"';
    await session.refresh();
    assert.equal(session.jobs, second);
    assert.notEqual(session.error, "");
  }

  malformed = false;
  jobs = null;
  await session.refresh();
  assert.deepEqual(session.jobs, []);
  assert.equal(session.error, "");
  expectedTag = tag;
  status = 304;
  const empty = session.jobs;
  await session.refresh();
  assert.equal(session.jobs, empty);
  assert.equal(session.error, "");
});
