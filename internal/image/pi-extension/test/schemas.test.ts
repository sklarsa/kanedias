import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { Value } from "typebox/value";
import {
  childResultSchema,
  delegateSessionSchema,
  handoffSchema,
  isDelegateSessionInput,
  isHandoffInput,
} from "../src/schemas.ts";

const fixtures = new URL("../../../supervisor/contract/testdata/", import.meta.url);

for (const name of ["create-child-read.json", "create-child-write.json"]) {
  test(`delegate schema consumes Go fixture ${name}`, async () => {
    const fixture = JSON.parse(await readFile(new URL(name, fixtures), "utf8"));
    const modelInput = { workerType: fixture.workerType, kind: fixture.kind, context: fixture.context, task: fixture.task };
    assert.equal(Value.Check(delegateSessionSchema, modelInput), true);
  });
}

for (const name of ["read-result.json", "write-result.json"]) {
  test(`result schema consumes strict Go fixture ${name}`, async () => {
    const fixture = JSON.parse(await readFile(new URL(name, fixtures), "utf8"));
    assert.equal(Value.Check(childResultSchema, fixture), true);
    assert.equal(Value.Check(childResultSchema, { ...fixture, credential: "must-not-pass" }), false);
  });
}

test("tool schemas stay within the portable cross-provider JSON Schema subset", () => {
  const patternPaths: string[] = [];
  const visit = (value: unknown, path: string): void => {
    if (Array.isArray(value)) {
      value.forEach((item, index) => visit(item, `${path}[${index}]`));
      return;
    }
    if (!value || typeof value !== "object") return;
    for (const [key, child] of Object.entries(value)) {
      if (key === "pattern") patternPaths.push(`${path}.${key}`);
      else visit(child, `${path}.${key}`);
    }
  };
  visit(delegateSessionSchema, "delegateSessionSchema");
  visit(handoffSchema, "handoffSchema");

  assert.deepEqual(patternPaths, [], "regex patterns are not portable across tool-calling grammar implementations");
});

test("categorical tool fields use cross-provider string enums", () => {
  assert.deepEqual(delegateSessionSchema.properties.kind, { type: "string", enum: ["read", "write"] });
  assert.deepEqual(delegateSessionSchema.properties.context, { type: "string", enum: ["fresh", "fork"] });
});

test("delegate input validation rejects whitespace-only semantic fields", () => {
  const valid = { workerType: "reviewer", kind: "read", context: "fresh", task: "Review it" };
  assert.equal(isDelegateSessionInput(valid), true);
  assert.equal(isDelegateSessionInput({ ...valid, task: "  " }), false);
  assert.equal(isDelegateSessionInput({ ...valid, workerType: "\t" }), false);
});

test("delegate schema requires every field and rejects malformed calls", () => {
  const valid = { workerType: "reviewer", kind: "read", context: "fresh", task: "Review it" };
  for (const field of Object.keys(valid)) {
    const candidate = { ...valid } as Record<string, unknown>;
    delete candidate[field];
    assert.equal(Value.Check(delegateSessionSchema, candidate), false, `accepted missing ${field}`);
  }
  for (const candidate of [
    { ...valid, extra: true },
    { ...valid, kind: "root" },
    { ...valid, kind: "edit" },
    { ...valid, context: "root" },
    { ...valid, context: "copy" },
    { ...valid, workerType: "" },
  ]) assert.equal(Value.Check(delegateSessionSchema, candidate), false, JSON.stringify(candidate));
});

test("handoff input validation rejects whitespace-only semantic fields", () => {
  const valid = {
    repositories: [{ path: "/workspace/repo", repository: "owner/repo", baseCommit: "abc", branch: "feature", headCommit: "def" }],
    summary: "Implemented it",
    verification: ["npm test"],
  };
  assert.equal(isHandoffInput(valid), true);
  assert.equal(isHandoffInput({ ...valid, summary: "\n" }), false);
  assert.equal(isHandoffInput({ ...valid, verification: ["  "] }), false);
  assert.equal(isHandoffInput({ ...valid, repositories: [{ ...valid.repositories[0], branch: "\t" }] }), false);
});

test("handoff schema is strict and includes local checkout paths", () => {
  const valid = {
    repositories: [{ path: "/workspace/repo", repository: "owner/repo", baseCommit: "abc", branch: "feature", headCommit: "def" }],
    summary: "Implemented it",
    verification: ["npm test"],
  };
  assert.equal(Value.Check(handoffSchema, valid), true);
  assert.equal(Value.Check(handoffSchema, { ...valid, extra: true }), false);
  assert.equal(Value.Check(handoffSchema, { ...valid, repositories: [{ ...valid.repositories[0], path: "" }] }), false);
  assert.equal(Value.Check(handoffSchema, { ...valid, summary: "" }), false);
});
