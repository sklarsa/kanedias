import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { SessionManager } from "@earendil-works/pi-coding-agent";
import { prepareFork } from "../src/fork.ts";

const usage = { input: 1, output: 1, cacheRead: 0, cacheWrite: 0, totalTokens: 2, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
async function sessionFixture() {
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-fork-"));
  const file = path.join(dir, "parent.jsonl");
  const lines = [
    { type: "session", version: 3, id: "parent-session", timestamp: new Date(0).toISOString(), cwd: "/workspace" },
    { type: "message", id: "u1", parentId: null, timestamp: new Date(1).toISOString(), message: { role: "user", content: [{ type: "text", text: "keep user" }], timestamp: 1 } },
    { type: "message", id: "a1", parentId: "u1", timestamp: new Date(2).toISOString(), message: { role: "assistant", provider: "google", model: "gemini", api: "google-generative-ai", content: [{ type: "thinking", thinking: "secret reasoning", thinkingSignature: "signed" }, { type: "text", text: "keep text", textSignature: "google-signed-text" }, { type: "toolCall", id: "call", name: "read", arguments: { path: "x" } }], usage, stopReason: "toolUse", timestamp: 2 } },
    { type: "message", id: "tr1", parentId: "a1", timestamp: new Date(3).toISOString(), message: { role: "toolResult", toolCallId: "call", toolName: "read", content: [{ type: "text", text: "keep tool result" }], isError: false, timestamp: 3 } },
    { type: "message", id: "other", parentId: "u1", timestamp: new Date(4).toISOString(), message: { role: "user", content: [{ type: "text", text: "exclude branch" }], timestamp: 4 } },
  ];
  await writeFile(file, lines.map((line) => JSON.stringify(line)).join("\n") + "\n", { mode: 0o600 });
  return { dir, file, before: await readFile(file) };
}

test("fork leaves parent unchanged, creates a new child identity, and ends at leaf", async (t) => {
  const fixture = await sessionFixture(); t.after(() => rm(fixture.dir, { recursive: true, force: true }));
  const fork = await prepareFork(fixture.file, "tr1", { provider: "google", model: "gemini" });
  assert.deepEqual(await readFile(fixture.file), fixture.before);
  assert.notEqual(fork.piSessionId, "parent-session");
  const lines = (await readFile(fork.sessionFile, "utf8")).trim().split("\n").map((line) => JSON.parse(line));
  assert.equal(lines[0].parentSession, fixture.file);
  assert.equal(lines.at(-1).id, "tr1");
  assert.equal(JSON.stringify(lines).includes("exclude branch"), false);
  assert.equal(JSON.stringify(lines).includes("signed"), true);
  assert.equal(JSON.stringify(lines).includes("google-signed-text"), true);
});

test("fork removes complete incompatible signed-thinking blocks and text signatures but keeps normal and tool history", async (t) => {
  const fixture = await sessionFixture(); t.after(() => rm(fixture.dir, { recursive: true, force: true }));
  const fork = await prepareFork(fixture.file, "tr1", { provider: "openai", model: "gpt" });
  const text = await readFile(fork.sessionFile, "utf8");
  assert.equal(text.includes("secret reasoning"), false);
  assert.equal(text.includes("google-signed-text"), false);
  for (const kept of ["keep user", "keep text", "toolCall", "keep tool result"]) assert.equal(text.includes(kept), true, kept);
});

test("fork preserves labels, validates the selected conversation path, and returns the reopenable persisted leaf", async (t) => {
  const fixture = await sessionFixture(); t.after(() => rm(fixture.dir, { recursive: true, force: true }));
  const sourceLines = (await readFile(fixture.file, "utf8")).trim().split("\n").map((line) => JSON.parse(line));
  sourceLines.splice(3, 0, { type: "label", id: "path-label", parentId: "a1", timestamp: new Date(2.5).toISOString(), targetId: "a1", label: "assistant turn" });
  sourceLines.find((line) => line.id === "tr1").parentId = "path-label";
  sourceLines.splice(5, 0, { type: "label", id: "current-label", parentId: "tr1", timestamp: new Date(3.5).toISOString(), targetId: "tr1", label: "selected turn" });
  await writeFile(fixture.file, sourceLines.map((line) => JSON.stringify(line)).join("\n") + "\n", { mode: 0o600 });
  const before = await readFile(fixture.file);

  for (const selectedLeaf of ["tr1", "current-label"]) {
    const fork = await prepareFork(fixture.file, selectedLeaf, { provider: "google", model: "gemini" });
    assert.deepEqual(await readFile(fixture.file), before);
    const reopened = SessionManager.open(fork.sessionFile);
    assert.equal(reopened.getLeafId(), fork.leafEntryId);
    assert.deepEqual(reopened.getBranch(fork.leafEntryId).filter((entry) => entry.type !== "label").map((entry) => entry.id), ["u1", "a1", "tr1"]);
    const branchLines = (await readFile(fork.sessionFile, "utf8")).trim().split("\n").map((line) => JSON.parse(line));
    assert.equal(branchLines[0].parentSession, fixture.file);
    assert.deepEqual(branchLines.filter((line) => line.type === "label").map(({ targetId, label, timestamp }) => ({ targetId, label, timestamp })), [
      { targetId: "a1", label: "assistant turn", timestamp: new Date(2.5).toISOString() },
      { targetId: "tr1", label: "selected turn", timestamp: new Date(3.5).toISOString() },
    ]);
  }
});

test("legacy migration happens only on an isolated copy and leaves the original bytes unchanged", async (t) => {
  const fixture = await sessionFixture(); t.after(() => rm(fixture.dir, { recursive: true, force: true }));
  const legacy = (await readFile(fixture.file, "utf8")).replace('"version":3', '"version":2');
  await writeFile(fixture.file, legacy, { mode: 0o600 });
  const before = await readFile(fixture.file);
  const fork = await prepareFork(fixture.file, "tr1", { provider: "google", model: "gemini" });
  assert.deepEqual(await readFile(fixture.file), before);
  assert.equal(JSON.parse((await readFile(fork.sessionFile, "utf8")).split("\n")[0]!).version, 3);
});

test("malformed, missing-leaf, and unpersisted sources fail without changing original bytes", async (t) => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "kanedias-bad-fork-")); t.after(() => rm(dir, { recursive: true, force: true }));
  const malformed = path.join(dir, "bad.jsonl"); await writeFile(malformed, "not json\n");
  const malformedBefore = await readFile(malformed);
  await assert.rejects(prepareFork(malformed, "leaf", { provider: "x", model: "y" }));
  assert.deepEqual(await readFile(malformed), malformedBefore);
  const fixture = await sessionFixture(); t.after(() => rm(fixture.dir, { recursive: true, force: true }));
  await assert.rejects(prepareFork(fixture.file, "missing", { provider: "x", model: "y" }));
  await assert.rejects(prepareFork("", "leaf", { provider: "x", model: "y" }), /persisted/);
});
