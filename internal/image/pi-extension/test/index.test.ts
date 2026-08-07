import assert from "node:assert/strict";
import { execFile, spawn } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import readline from "node:readline";
import test from "node:test";
import { promisify } from "node:util";
import extension from "../src/index.ts";

type Tool = { name: string; execute: (...args: any[]) => Promise<any> };
const runFile = promisify(execFile);
async function server(handler: http.RequestListener) {
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-tools-")); const socket = path.join(dir, "s.sock");
  const instance = http.createServer(handler); await new Promise<void>((resolve, reject) => instance.listen(socket, resolve).once("error", reject)); await chmod(socket, 0o600);
  return { socket, close: async () => { instance.closeAllConnections(); await new Promise<void>((resolve) => instance.close(() => resolve())); await rm(dir, { recursive: true, force: true }); } };
}

test("extension registers exactly delegate_session and handoff and describes configured workers", async (t) => {
  const fixture = await server((_req, res) => { res.setHeader("content-type", "application/json"); res.end(JSON.stringify([{ workerType: "reviewer", description: "Reviews code", profile: { provider: "provider", model: "model" } }])); }); t.after(fixture.close);
  const tools: Tool[] = [];
  await extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SUPERVISOR_SOCKET: fixture.socket } });
  assert.deepEqual(tools.map((tool) => tool.name), ["delegate_session", "handoff"]);
  assert.match((tools[0] as any).description, /reviewer: Reviews code/);
});

test("fresh delegation discovers workers, posts canonical DTO, and returns typed details", async (t) => {
  const seen: unknown[] = [];
  const fixture = await server((req, res) => {
    res.setHeader("content-type", "application/json");
    if (req.url === "/v1/workers") return res.end(JSON.stringify([{ workerType: "reviewer", description: "Reviews", profile: { provider: "anthropic", model: "claude" } }]));
    let body = ""; req.on("data", (chunk) => body += chunk); req.on("end", () => { seen.push(JSON.parse(body)); res.end(JSON.stringify({ kind: "read", workerType: "reviewer", sessionId: "child", output: "review result" })); });
  }); t.after(fixture.close);
  const tools: Tool[] = []; await extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket } });
  const result = await tools[0]!.execute("call", { workerType: "reviewer", kind: "read", context: "fresh", task: "Review" }, undefined, undefined, { sessionManager: {} });
  assert.deepEqual(seen, [{ workerType: "reviewer", kind: "read", context: "fresh", task: "Review" }]);
  assert.equal(result.content[0].text, "review result");
  assert.equal(result.details.sessionId, "child");
});

test("fork delegation posts a new persisted branch and leaves the parent unchanged", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-index-fork-")); t.after(() => rm(dir, { recursive: true, force: true }));
  const parent = path.join(dir, "parent.jsonl");
  const lines = [
    { type: "session", version: 3, id: "parent-session", timestamp: new Date(0).toISOString(), cwd: "/workspace" },
    { type: "message", id: "u1", parentId: null, timestamp: new Date(1).toISOString(), message: { role: "user", content: [{ type: "text", text: "context" }], timestamp: 1 } },
    { type: "message", id: "a1", parentId: "u1", timestamp: new Date(2).toISOString(), message: { role: "assistant", provider: "provider", model: "model", api: "test", content: [{ type: "text", text: "answer" }], usage: { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } }, stopReason: "stop", timestamp: 2 } },
  ];
  await writeFile(parent, lines.map((line) => JSON.stringify(line)).join("\n") + "\n", { mode: 0o600 }); const before = await readFile(parent);
  let posted: any;
  const fixture = await server((req, res) => { res.setHeader("content-type", "application/json"); if (req.url === "/v1/workers") return res.end(JSON.stringify([{ workerType: "reviewer", description: "Reviews", profile: { provider: "provider", model: "model" } }])); let raw = ""; req.on("data", (chunk) => raw += chunk); req.on("end", () => { posted = JSON.parse(raw); res.end(JSON.stringify({ kind: "read", workerType: "reviewer", sessionId: "child", output: "done" })); }); }); t.after(fixture.close);
  const tools: Tool[] = []; await extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket, KANEDIAS_PI_SESSION_FILE: "" } });
  await tools[0]!.execute("call", { workerType: "reviewer", kind: "read", context: "fork", task: "Review" }, undefined, undefined, { sessionManager: { getSessionFile: () => parent, getLeafId: () => "a1" } });
  assert.deepEqual(await readFile(parent), before);
  assert.equal(posted.context, "fork"); assert.equal(posted.fork.leafEntryId, "a1"); assert.notEqual(posted.fork.sessionFile, parent); assert.notEqual(posted.fork.piSessionId, "parent-session");
});

test("empty, legacy-unpersisted, and mixed-malformed forks leave source bytes unchanged and make zero HTTP requests", async (t) => {
  let requests = 0;
  const fixture = await server((req, res) => { if (req.url !== "/v1/workers") requests++; res.setHeader("content-type", "application/json"); res.end(JSON.stringify([{ workerType: "reviewer", description: "Reviews", profile: { provider: "anthropic", model: "claude" } }])); }); t.after(fixture.close);
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
    await extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket, KANEDIAS_PI_SESSION_FILE: file } });
    await assert.rejects(tools[0]!.execute("call", { workerType: "reviewer", kind: "read", context: "fork", task: "Review" }, undefined, undefined, { sessionManager: { getSessionFile: () => file, getLeafId: () => input.leaf } }));
    assert.deepEqual(await readFile(file), before, input.name);
  }
  assert.equal(requests, 0);
});

test("an unpersisted fork fails before contacting the supervisor", async (t) => {
  let requests = 0;
  const fixture = await server((req, res) => { if (req.url !== "/v1/workers") requests++; res.setHeader("content-type", "application/json"); res.end("[]"); }); t.after(fixture.close);
  const tools: Tool[] = [];
  await extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket, KANEDIAS_PI_SESSION_FILE: "/missing/session.jsonl" } });
  await assert.rejects(tools[0]!.execute("call", { workerType: "reviewer", kind: "read", context: "fork", task: "Review" }, undefined, undefined, { sessionManager: { getSessionFile: () => "/missing/session.jsonl", getLeafId: () => "leaf" } }));
  assert.equal(requests, 0);
});

test("unknown workers fail before child provisioning", async (t) => {
  let childCalls = 0;
  const fixture = await server((req, res) => { res.setHeader("content-type", "application/json"); if (req.url === "/v1/workers") res.end("[]"); else { childCalls++; res.end("{}"); } }); t.after(fixture.close);
  const tools: Tool[] = []; await extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "parent", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket } });
  await assert.rejects(tools[0]!.execute("call", { workerType: "missing", kind: "read", context: "fresh", task: "Review" }, undefined, undefined, { sessionManager: {} }), /unknown worker/i);
  assert.equal(childCalls, 0);
});

test("handoff verifies refs, strips checkout paths, shuts down only after acceptance, and terminates", async (t) => {
  const workspace = await mkdtemp(path.join(os.tmpdir(), "kanedias-index-handoff-"));
  const checkout = path.join(workspace, "owner", "repo");
  await mkdir(checkout, { recursive: true });
  t.after(() => rm(workspace, { recursive: true, force: true }));
  let body: any; let accepted = true;
  const fixture = await server((req, res) => { res.setHeader("content-type", "application/json"); if (req.method === "GET") return res.end("[]"); let raw = ""; req.on("data", (c) => raw += c); req.on("end", () => { body = JSON.parse(raw); res.statusCode = accepted ? 200 : 409; res.end(accepted ? JSON.stringify({ accepted: true, sessionId: "writer" }) : JSON.stringify({ error: "rejected" })); }); }); t.after(fixture.close);
  const tools: Tool[] = [];
  const pi = {
    registerTool: (tool: Tool) => tools.push(tool),
    exec: async (_command: string, args: string[]) => {
      const operation = args.slice(2).join(" ");
      const stdout = operation === "rev-parse --show-toplevel" ? `${checkout}\n`
        : operation === "rev-parse HEAD" ? "def\n"
        : operation === "remote get-url origin" ? "git@github.com:owner/repo.git\n"
        : operation.startsWith("ls-remote") ? "def\trefs/heads/feature\n" : "";
      return { stdout, stderr: "", code: 0, killed: false };
    },
  } as any;
  await extension(pi, { env: { KANEDIAS_SESSION_ID: "writer", KANEDIAS_SESSION_KIND: "write", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket }, workspaceRoot: workspace });
  let shutdowns = 0;
  const args = { repositories: [{ path: checkout, repository: "owner/repo", baseCommit: "abc", branch: "feature", headCommit: "def" }], summary: "done", verification: ["npm test"] };
  const result = await tools[1]!.execute("call", args, undefined, undefined, { shutdown: () => shutdowns++ });
  assert.equal(JSON.stringify(body).includes("path"), false);
  assert.deepEqual(body.repositories[0], { repository: "owner/repo", baseCommit: "abc", branch: "feature", headCommit: "def" });
  assert.equal(result.terminate, true); assert.equal(shutdowns, 1);
  accepted = false;
  await assert.rejects(tools[1]!.execute("call", args, undefined, undefined, { shutdown: () => shutdowns++ }));
  assert.equal(shutdowns, 1);
});

test("Pi 0.83.0 invokes the real extension tools against a fake Unix supervisor", async (t) => {
  const childPosts: any[] = [];
  const handoffPosts: any[] = [];
  let cancelledRequest = false;
  let releaseCancellation: (() => void) | undefined;
  const childRequestStarted = new Promise<void>((resolve) => { releaseCancellation = resolve; });
  const fixture = await server((req, res) => {
    res.setHeader("content-type", "application/json");
    if (req.url === "/v1/workers") return res.end(JSON.stringify([{ workerType: "reviewer", description: "Reviews code exactly", profile: { provider: "provider", model: "model" } }]));
    let raw = "";
    req.on("data", (chunk) => raw += chunk);
    req.on("end", () => {
      const body = JSON.parse(raw);
      if (req.url === "/v1/handoff") {
        handoffPosts.push(body);
        res.end(JSON.stringify({ accepted: true, sessionId: "writer-real" }));
        return;
      }
      childPosts.push(body);
      if (body.task === "cancel through Pi") {
        req.on("aborted", () => { cancelledRequest = true; });
        res.on("close", () => { if (!res.writableEnded) cancelledRequest = true; });
        releaseCancellation?.();
        return;
      }
      res.end(JSON.stringify({ kind: "read", workerType: "reviewer", sessionId: `child-${childPosts.length}`, output: "read-through-real-pi" }));
    });
  });
  t.after(fixture.close);

  const providerRequests: any[] = [];
  let writerHandoffArgs: any;
  const provider = http.createServer((req, res) => {
    let raw = "";
    req.on("data", (chunk) => raw += chunk);
    req.on("end", () => {
      const body = JSON.parse(raw);
      providerRequests.push(body);
      const last = body.messages.at(-1);
      let delta: any;
      let finish = "stop";
      if (last?.role === "tool") {
        delta = { role: "assistant", content: "provider observed mapped tool result" };
      } else {
        const prompt = typeof last?.content === "string" ? last.content : Array.isArray(last?.content) ? last.content.map((part: any) => part.text ?? "").join("") : "";
        finish = "tool_calls";
        const tool = prompt.includes("handoff") ? "handoff" : "delegate_session";
        const args = tool === "handoff"
          ? writerHandoffArgs
          : { workerType: "reviewer", kind: "read", context: prompt.includes("fork") ? "fork" : "fresh", task: prompt.includes("cancel") ? "cancel through Pi" : `real Pi ${prompt.includes("fork") ? "fork" : "fresh"}` };
        delta = { role: "assistant", tool_calls: [{ index: 0, id: `call-${providerRequests.length}`, type: "function", function: { name: tool, arguments: JSON.stringify(args) } }] };
      }
      res.writeHead(200, { "content-type": "text/event-stream" });
      res.write(`data: ${JSON.stringify({ id: "chat", object: "chat.completion.chunk", created: 1, model: "deterministic", choices: [{ index: 0, delta, finish_reason: null }] })}\n\n`);
      res.write(`data: ${JSON.stringify({ id: "chat", object: "chat.completion.chunk", created: 1, model: "deterministic", choices: [{ index: 0, delta: {}, finish_reason: finish }] })}\n\n`);
      res.end("data: [DONE]\n\n");
    });
  });
  await new Promise<void>((resolve, reject) => provider.listen(0, "127.0.0.1", resolve).once("error", reject));
  t.after(() => new Promise<void>((resolve) => provider.close(() => resolve())));
  const address = provider.address();
  if (!address || typeof address === "string") throw new Error("test provider did not bind TCP");

  const home = await mkdtemp(path.join(os.tmpdir(), "kanedias-real-pi-"));
  t.after(() => rm(home, { recursive: true, force: true }));
  const child = spawn(path.resolve("node_modules/.bin/pi"), [
    "--mode", "rpc", "--no-builtin-tools", "--provider", "kanedias-test", "--model", "deterministic",
    "-e", path.resolve("test/fixtures/deterministic-provider.ts"), "-e", path.resolve("test/fixtures/kanedias-test-entry.ts"),
  ], {
    env: { ...process.env, HOME: home, KANEDIAS_TEST_PROVIDER_URL: `http://127.0.0.1:${address.port}/v1`, KANEDIAS_SESSION_ID: "root-real", KANEDIAS_SESSION_KIND: "root", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket, KANEDIAS_PI_SESSION_FILE: "" },
    stdio: ["pipe", "pipe", "pipe"],
  });
  let stderr = "";
  child.stderr.on("data", (chunk) => stderr += chunk);
  const messages: any[] = [];
  readline.createInterface({ input: child.stdout }).on("line", (line) => messages.push(JSON.parse(line)));
  async function waitForMessage(predicate: (message: any) => boolean, description: string): Promise<any> {
    const deadline = Date.now() + 10_000;
    while (Date.now() < deadline) {
      const found = messages.find(predicate);
      if (found) return found;
      await new Promise((resolve) => setTimeout(resolve, 5));
    }
    throw new Error(`timed out waiting for ${description}: ${stderr}\n${JSON.stringify(messages)}`);
  }
  let sequence = 0;
  async function command(command: Record<string, unknown>): Promise<any> {
    const id = `rpc-${++sequence}`;
    child.stdin.write(JSON.stringify({ id, ...command }) + "\n");
    return waitForMessage((message) => message.id === id, `${String(command.type)} response`);
  }
  async function waitIdle(): Promise<void> {
    for (;;) {
      const state = await command({ type: "get_state" });
      if (state.success && !state.data.isStreaming) return;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  const stopChild = async () => {
    if (child.exitCode !== null) return;
    child.stdin.end();
    await new Promise<void>((resolve, reject) => { const timer = setTimeout(() => { child.kill("SIGKILL"); reject(new Error(`Pi did not exit: ${stderr}`)); }, 10_000); child.once("exit", (code) => { clearTimeout(timer); code === 0 ? resolve() : reject(new Error(`Pi exited ${code}: ${stderr}`)); }); });
  };
  t.after(stopChild);

  const state = await command({ type: "get_state" });
  assert.equal(state.success, true);
  const parentFile = state.data.sessionFile as string;

  assert.equal((await command({ type: "prompt", message: "delegate fresh" })).success, true);
  await waitIdle();
  assert.deepEqual(childPosts[0], { workerType: "reviewer", kind: "read", context: "fresh", task: "real Pi fresh" });
  const firstProviderRequest = providerRequests[0];
  assert.deepEqual(firstProviderRequest.tools.map((tool: any) => tool.function.name), ["delegate_session", "handoff"]);
  assert.match(firstProviderRequest.tools[0].function.description, /reviewer: Reviews code exactly/);
  const mappedResult = providerRequests.find((request) => request.messages.at(-1)?.role === "tool");
  assert.match(JSON.stringify(mappedResult.messages.at(-1)), /read-through-real-pi/);

  const parentPrefix = await readFile(parentFile);
  assert.equal((await command({ type: "prompt", message: "delegate fork" })).success, true);
  await waitIdle();
  assert.equal(childPosts[1].context, "fork");
  assert.equal(typeof childPosts[1].fork.piSessionId, "string");
  assert.notEqual(childPosts[1].fork.sessionFile, parentFile);
  const parentAfterFork = await readFile(parentFile);
  assert.deepEqual(parentAfterFork.subarray(0, parentPrefix.length), parentPrefix);

  assert.equal((await command({ type: "prompt", message: "delegate cancel" })).success, true);
  await childRequestStarted;
  assert.equal((await command({ type: "abort" })).success, true);
  await waitIdle();
  assert.equal(cancelledRequest, true);

  await stopChild();

  const writerHome = await mkdtemp(path.join(os.tmpdir(), "kanedias-real-pi-writer-"));
  const writerWorkspace = await mkdtemp(path.join(os.tmpdir(), "kanedias-real-pi-repos-"));
  t.after(() => rm(writerHome, { recursive: true, force: true }));
  t.after(() => rm(writerWorkspace, { recursive: true, force: true }));
  const checkout = path.join(writerWorkspace, "owner", "repo");
  const remote = path.join(writerWorkspace, "remotes", "owner", "repo.git");
  await mkdir(checkout, { recursive: true });
  await mkdir(remote, { recursive: true });
  await runFile("git", ["init", "--bare", remote]);
  await runFile("git", ["init", checkout]);
  await runFile("git", ["-C", checkout, "config", "user.email", "test@example.com"]);
  await runFile("git", ["-C", checkout, "config", "user.name", "Test"]);
  await writeFile(path.join(checkout, "file.txt"), "writer\n");
  await runFile("git", ["-C", checkout, "add", "file.txt"]);
  await runFile("git", ["-C", checkout, "commit", "-m", "writer"]);
  await runFile("git", ["-C", checkout, "switch", "-c", "feature"]);
  await runFile("git", ["-C", checkout, "remote", "add", "origin", remote]);
  await runFile("git", ["-C", checkout, "push", "-u", "origin", "feature"]);
  const head = (await runFile("git", ["-C", checkout, "rev-parse", "HEAD"])).stdout.trim();
  writerHandoffArgs = { repositories: [{ path: checkout, repository: "owner/repo", baseCommit: head, branch: "feature", headCommit: head }], summary: "done", verification: ["tests"] };

  const writer = spawn(path.resolve("node_modules/.bin/pi"), [
    "--mode", "rpc", "--no-builtin-tools", "--provider", "kanedias-test", "--model", "deterministic",
    "-e", path.resolve("test/fixtures/deterministic-provider.ts"), "-e", path.resolve("test/fixtures/kanedias-test-entry.ts"),
  ], { env: { ...process.env, HOME: writerHome, KANEDIAS_TEST_PROVIDER_URL: `http://127.0.0.1:${address.port}/v1`, KANEDIAS_TEST_WORKSPACE_ROOT: writerWorkspace, KANEDIAS_SESSION_ID: "writer-real", KANEDIAS_SESSION_KIND: "write", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket }, stdio: ["pipe", "pipe", "pipe"] });
  t.after(() => { if (writer.exitCode === null) writer.kill("SIGKILL"); });
  let writerStderr = ""; writer.stderr.on("data", (chunk) => writerStderr += chunk);
  const writerMessages: any[] = []; readline.createInterface({ input: writer.stdout }).on("line", (line) => writerMessages.push(JSON.parse(line)));
  writer.stdin.write(JSON.stringify({ id: "handoff", type: "prompt", message: "invoke handoff" }) + "\n");
  await new Promise<void>((resolve, reject) => {
    const timer = setTimeout(() => { writer.kill("SIGKILL"); reject(new Error(`writer Pi did not exit after accepted handoff: ${writerStderr}\n${JSON.stringify(writerMessages)}`)); }, 10_000);
    writer.once("exit", (code) => { clearTimeout(timer); code === 0 ? resolve() : reject(new Error(`writer Pi exited ${code}: ${writerStderr}`)); });
  });
  assert.equal(handoffPosts.length, 1);
  assert.equal(JSON.stringify(handoffPosts[0]).includes("path"), false);
  assert.deepEqual(handoffPosts[0].repositories, [{ repository: "owner/repo", baseCommit: head, branch: "feature", headCommit: head }]);
});

test("handoff is unavailable outside write sessions", async (t) => {
  const fixture = await server((_req, res) => { res.setHeader("content-type", "application/json"); res.end("[]"); }); t.after(fixture.close);
  const tools: Tool[] = []; await extension({ registerTool: (tool: Tool) => tools.push(tool) } as any, { env: { KANEDIAS_SESSION_ID: "root", KANEDIAS_SESSION_KIND: "root", KANEDIAS_SUPERVISOR_SOCKET: fixture.socket } });
  await assert.rejects(tools[1]!.execute("call", { repositories: [], summary: "done", verification: [] }, undefined, undefined, { shutdown() {} }), /write session/);
});
