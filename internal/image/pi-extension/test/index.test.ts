import assert from "node:assert/strict";
import { chmod, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import extension from "../src/index.ts";

type Tool = { name: string; execute: (...args: any[]) => Promise<any> };
async function server(handler: http.RequestListener) {
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-tools-")); const socket = path.join(dir, "s.sock");
  const instance = http.createServer(handler); await new Promise<void>((resolve, reject) => instance.listen(socket, resolve).once("error", reject)); await chmod(socket, 0o600);
  return { socket, close: async () => { await new Promise<void>((resolve) => instance.close(() => resolve())); await rm(dir, { recursive: true, force: true }); } };
}

test("extension registers exactly delegate_session and handoff", () => {
  const tools: Tool[] = [];
  extension({ registerTool: (tool: Tool) => tools.push(tool) } as any);
  assert.deepEqual(tools.map((tool) => tool.name), ["delegate_session", "handoff"]);
});

test("fresh delegation discovers workers, posts canonical DTO, and returns typed details", async (t) => {
  const seen: unknown[] = [];
  const fixture = await server((req, res) => {
    res.setHeader("content-type", "application/json");
    if (req.url === "/v1/workers") return res.end(JSON.stringify([{ workerType: "reviewer", description: "Reviews", profile: { provider: "anthropic", model: "claude" } }]));
    let body = ""; req.on("data", (chunk) => body += chunk); req.on("end", () => { seen.push(JSON.parse(body)); res.end(JSON.stringify({ kind: "read", workerType: "reviewer", sessionId: "child", output: "review result" })); });
  }); t.after(fixture.close);
  const tools: Tool[] = []; extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket } });
  const result = await tools[0]!.execute("call", { workerType: "reviewer", kind: "read", context: "fresh", task: "Review" }, undefined, undefined, { sessionManager: {} });
  assert.deepEqual(seen, [{ workerType: "reviewer", kind: "read", context: "fresh", task: "Review" }]);
  assert.equal(result.content[0].text, "review result");
  assert.equal(result.details.sessionId, "child");
});

test("empty, legacy-unpersisted, and mixed-malformed forks leave source bytes unchanged and make zero HTTP requests", async (t) => {
  let requests = 0;
  const fixture = await server((_req, res) => { requests++; res.setHeader("content-type", "application/json"); res.end(JSON.stringify([{ workerType: "reviewer", description: "Reviews", profile: { provider: "anthropic", model: "claude" } }])); }); t.after(fixture.close);
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-preflight-")); t.after(() => rm(dir, { recursive: true, force: true }));
  const files = [
    { name: "empty.jsonl", content: "", leaf: "leaf" },
    { name: "legacy.jsonl", content: JSON.stringify({ type: "session", version: 1, id: "legacy", timestamp: new Date(0).toISOString(), cwd: "/workspace" }) + "\n" + JSON.stringify({ type: "message", timestamp: new Date(1).toISOString(), message: { role: "assistant", content: [{ type: "text", text: "legacy" }] } }) + "\n", leaf: "legacy-leaf" },
    { name: "mixed.jsonl", content: [JSON.stringify({ type: "session", version: 3, id: "parent", timestamp: new Date(0).toISOString(), cwd: "/workspace" }), JSON.stringify({ type: "message", id: "leaf", parentId: null, timestamp: new Date(1).toISOString(), message: { role: "assistant", provider: "anthropic", model: "claude", content: [{ type: "text", text: "valid" }] } }), "{malformed"].join("\n") + "\n", leaf: "leaf" },
  ];

  for (const input of files) {
    const file = path.join(dir, input.name); await writeFile(file, input.content, { mode: 0o600 });
    const before = await readFile(file);
    const tools: Tool[] = [];
    extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket, KANEDIAS_PI_SESSION_FILE: file } });
    await assert.rejects(tools[0]!.execute("call", { workerType: "reviewer", kind: "read", context: "fork", task: "Review" }, undefined, undefined, { sessionManager: { getSessionFile: () => file, getLeafId: () => input.leaf } }));
    assert.deepEqual(await readFile(file), before, input.name);
  }
  assert.equal(requests, 0);
});

test("an unpersisted fork fails before contacting the supervisor", async (t) => {
  let requests = 0;
  const fixture = await server((_req, res) => { requests++; res.setHeader("content-type", "application/json"); res.end("[]"); }); t.after(fixture.close);
  const tools: Tool[] = [];
  extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket, KANEDIAS_PI_SESSION_FILE: "/missing/session.jsonl" } });
  await assert.rejects(tools[0]!.execute("call", { workerType: "reviewer", kind: "read", context: "fork", task: "Review" }, undefined, undefined, { sessionManager: { getSessionFile: () => "/missing/session.jsonl", getLeafId: () => "leaf" } }));
  assert.equal(requests, 0);
});

test("unknown workers fail before child provisioning", async (t) => {
  let childCalls = 0;
  const fixture = await server((req, res) => { res.setHeader("content-type", "application/json"); if (req.url === "/v1/workers") res.end("[]"); else { childCalls++; res.end("{}"); } }); t.after(fixture.close);
  const tools: Tool[] = []; extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket } });
  await assert.rejects(tools[0]!.execute("call", { workerType: "missing", kind: "read", context: "fresh", task: "Review" }, undefined, undefined, { sessionManager: {} }), /unknown worker/i);
  assert.equal(childCalls, 0);
});

test("handoff strips checkout paths, shuts down only after acceptance, and terminates", async (t) => {
  let body: any; let accepted = true;
  const fixture = await server((req, res) => { let raw = ""; req.on("data", (c) => raw += c); req.on("end", () => { body = JSON.parse(raw); res.statusCode = accepted ? 200 : 409; res.setHeader("content-type", "application/json"); res.end(accepted ? "{}" : JSON.stringify({ error: "rejected" })); }); }); t.after(fixture.close);
  const tools: Tool[] = []; extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "writer", KANEDIAS_SESSION_KIND: "write", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket } });
  let shutdowns = 0;
  const args = { repositories: [{ path: "/workspace/repo", repository: "owner/repo", baseCommit: "abc", branch: "feature", headCommit: "def" }], summary: "done", verification: ["npm test"] };
  const result = await tools[1]!.execute("call", args, undefined, undefined, { shutdown: () => shutdowns++ });
  assert.equal(JSON.stringify(body).includes("path"), false);
  assert.deepEqual(body.repositories[0], { repository: "owner/repo", baseCommit: "abc", branch: "feature", headCommit: "def" });
  assert.equal(result.terminate, true); assert.equal(shutdowns, 1);
  accepted = false;
  await assert.rejects(tools[1]!.execute("call", args, undefined, undefined, { shutdown: () => shutdowns++ }));
  assert.equal(shutdowns, 1);
});

test("handoff is unavailable outside write sessions", async () => {
  const tools: Tool[] = []; extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "root", KANEDIAS_SESSION_KIND: "root", KANEDIAS_SUPERVISOR_SOCKET: "/missing" } });
  await assert.rejects(tools[1]!.execute("call", { repositories: [], summary: "done", verification: [] }, undefined, undefined, { shutdown() {} }), /write session/);
});
