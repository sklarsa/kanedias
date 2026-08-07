import assert from "node:assert/strict";
import { chmod, mkdtemp, rm } from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { SupervisorClient } from "../src/supervisor-client.ts";

async function socketServer(handler: http.RequestListener) {
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-client-"));
  const socket = path.join(dir, "supervisor.sock");
  const server = http.createServer(handler);
  await new Promise<void>((resolve, reject) => server.listen(socket, resolve).once("error", reject));
  await chmod(socket, 0o600);
  return { socket, close: async () => { await new Promise<void>((resolve) => server.close(() => resolve())); await rm(dir, { recursive: true, force: true }); } };
}

test("client uses only a mode-0600 Unix socket and JSON", async (t) => {
  const fixture = await socketServer((req, res) => {
    assert.equal(req.url, "/v1/workers");
    assert.equal(req.headers.accept, "application/json");
    res.setHeader("content-type", "application/json; charset=utf-8");
    res.end(JSON.stringify([{ workerType: "reviewer", description: "Reviews", profile: { provider: "anthropic", model: "claude" } }]));
  });
  t.after(fixture.close);
  const workers = await new SupervisorClient(fixture.socket).workers();
  assert.equal(workers[0]?.workerType, "reviewer");
  await chmod(fixture.socket, 0o666);
  await assert.rejects(new SupervisorClient(fixture.socket).workers(), /0600/);
});

test("client rejects non-JSON, redirects, and bodies over 1 MiB", async (t) => {
  let response: "text" | "redirect" | "large" = "text";
  const fixture = await socketServer((_req, res) => {
    if (response === "redirect") { res.statusCode = 302; res.setHeader("location", "http://127.0.0.1:1/steal"); res.setHeader("content-type", "application/json"); res.end("{}"); return; }
    if (response === "large") { res.setHeader("content-type", "application/json"); res.end(JSON.stringify("x".repeat(1024 * 1024))); return; }
    res.setHeader("content-type", "text/plain"); res.end("{}");
  });
  t.after(fixture.close);
  const client = new SupervisorClient(fixture.socket);
  await assert.rejects(client.workers(), /content-type/);
  response = "redirect";
  await assert.rejects(client.workers(), /302/);
  response = "large";
  await assert.rejects(client.workers(), /1 MiB/);
});

test("discovery is timed out but blocking delegation has no overall timeout", async (t) => {
  const fixture = await socketServer((req, res) => {
    res.setHeader("content-type", "application/json");
    if (req.url === "/v1/workers") return;
    setTimeout(() => res.end(JSON.stringify({ kind: "read", workerType: "reviewer", sessionId: "child", output: "done" })), 80);
  });
  t.after(fixture.close);
  const client = new SupervisorClient(fixture.socket, { boundedTimeoutMs: 30 });
  await assert.rejects(client.workers(), /timed out/);
  const result = await client.createChild("parent", { workerType: "reviewer", kind: "read", context: "fresh", task: "review" });
  assert.equal(result.kind, "read");
});

test("abort destroys a blocking delegation request and surfaces cancellation", async (t) => {
  let requestClosed = false;
  const fixture = await socketServer((req) => req.on("close", () => { requestClosed = true; }));
  t.after(fixture.close);
  const abort = new AbortController();
  const pending = new SupervisorClient(fixture.socket).createChild("parent", { workerType: "reviewer", kind: "read", context: "fresh", task: "review" }, abort.signal);
  setTimeout(() => abort.abort(), 20);
  await assert.rejects(pending, /abort|cancel/i);
  await new Promise((resolve) => setTimeout(resolve, 20));
  assert.equal(requestClosed, true);
});
