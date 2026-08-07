import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { prepareFork } from "../src/fork.ts";

const usage = { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
async function sessionFixture() {
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-fork-"));
  const file = path.join(dir, "parent.jsonl");
  const lines = [
    { type: "session", version: 3, id: "parent-session", timestamp: new Date(0).toISOString(), cwd: "/workspace" },
    { type: "message", id: "u1", parentId: null, timestamp: new Date(1).toISOString(), message: { role: "user", content: [{ type: "text", text: "keep user" }], timestamp: 1 } },
    { type: "message", id: "a1", parentId: "u1", timestamp: new Date(2).toISOString(), message: { role: "assistant", provider: "anthropic", model: "claude", api: "anthropic-messages", content: [{ type: "thinking", thinking: "secret reasoning", thinkingSignature: "signed" }, { type: "text", text: "keep text" }, { type: "toolCall", id: "call", name: "read", arguments: { path: "x" } }], usage, stopReason: "toolUse", timestamp: 2 } },
    { type: "message", id: "tr1", parentId: "a1", timestamp: new Date(3).toISOString(), message: { role: "toolResult", toolCallId: "call", toolName: "read", content: [{ type: "text", text: "keep tool result" }], isError: false, timestamp: 3 } },
    { type: "message", id: "other", parentId: "u1", timestamp: new Date(4).toISOString(), message: { role: "user", content: [{ type: "text", text: "exclude branch" }], timestamp: 4 } },
  ];
  await writeFile(file, lines.map((line) => JSON.stringify(line)).join("\n") + "\n", { mode: 0o600 });
  return { dir, file, before: await readFile(file) };
}

test("fork leaves parent unchanged, creates a new child identity, and ends at leaf", async (t) => {
  const fixture = await sessionFixture(); t.after(() => rm(fixture.dir, { recursive: true, force: true }));
  const fork = await prepareFork(fixture.file, "tr1", { provider: "anthropic", model: "claude" });
  assert.deepEqual(await readFile(fixture.file), fixture.before);
  assert.notEqual(fork.piSessionId, "parent-session");
  const lines = (await readFile(fork.sessionFile, "utf8")).trim().split("\n").map((line) => JSON.parse(line));
  assert.equal(lines[0].parentSession, fixture.file);
  assert.equal(lines.at(-1).id, "tr1");
  assert.equal(JSON.stringify(lines).includes("exclude branch"), false);
  assert.equal(JSON.stringify(lines).includes("signed"), true);
});

test("fork removes complete incompatible signed-thinking blocks but keeps normal and tool history", async (t) => {
  const fixture = await sessionFixture(); t.after(() => rm(fixture.dir, { recursive: true, force: true }));
  const fork = await prepareFork(fixture.file, "tr1", { provider: "openai", model: "gpt" });
  const text = await readFile(fork.sessionFile, "utf8");
  assert.equal(text.includes("secret reasoning"), false);
  for (const kept of ["keep user", "keep text", "toolCall", "keep tool result"]) assert.equal(text.includes(kept), true, kept);
});

test("malformed, missing-leaf, and unpersisted sources fail", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-bad-fork-")); t.after(() => rm(dir, { recursive: true, force: true }));
  const malformed = path.join(dir, "bad.jsonl"); await writeFile(malformed, "not json\n");
  await assert.rejects(prepareFork(malformed, "leaf", { provider: "x", model: "y" }));
  const fixture = await sessionFixture(); t.after(() => rm(fixture.dir, { recursive: true, force: true }));
  await assert.rejects(prepareFork(fixture.file, "missing", { provider: "x", model: "y" }));
  await assert.rejects(prepareFork("", "leaf", { provider: "x", model: "y" }), /persisted/);
});
