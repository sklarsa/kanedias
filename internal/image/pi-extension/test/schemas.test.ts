import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { Value } from "typebox/value";
import { childResultSchema, delegateSessionSchema, handoffSchema } from "../src/schemas.ts";

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
    { ...valid, task: "  " },
    { ...valid, workerType: "" },
  ]) assert.equal(Value.Check(delegateSessionSchema, candidate), false, JSON.stringify(candidate));
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
