import { StringEnum } from "@earendil-works/pi-ai";
import { Type } from "typebox";
import { Value } from "typebox/value";
import type { DelegateSessionInput, HandoffInput } from "./types.ts";

const nonEmpty = Type.String({ minLength: 1 });

export const delegateSessionSchema = Type.Object({
  workerType: nonEmpty,
  kind: StringEnum(["read", "write"] as const),
  context: StringEnum(["fresh", "fork"] as const),
  task: nonEmpty,
}, { additionalProperties: false });

export function isDelegateSessionInput(input: unknown): input is DelegateSessionInput {
  if (!Value.Check(delegateSessionSchema, input)) return false;
  const candidate = input as DelegateSessionInput;
  return candidate.workerType.trim() !== "" && candidate.task.trim() !== "";
}

const repositoryHandoffInputSchema = Type.Object({
  path: nonEmpty,
  repository: nonEmpty,
  baseCommit: nonEmpty,
  branch: nonEmpty,
  headCommit: nonEmpty,
}, { additionalProperties: false });

export const handoffSchema = Type.Object({
  repositories: Type.Array(repositoryHandoffInputSchema, { minItems: 1 }),
  summary: nonEmpty,
  verification: Type.Array(nonEmpty),
}, { additionalProperties: false });

export function isHandoffInput(input: unknown): input is HandoffInput {
  if (!Value.Check(handoffSchema, input)) return false;
  const candidate = input as HandoffInput;
  return candidate.summary.trim() !== ""
    && candidate.verification.every((entry) => entry.trim() !== "")
    && candidate.repositories.every((repository) =>
      repository.path.trim() !== ""
      && repository.repository.trim() !== ""
      && repository.baseCommit.trim() !== ""
      && repository.branch.trim() !== ""
      && repository.headCommit.trim() !== "");
}

const repositoryHandoffSchema = Type.Object({
  repository: Type.String(),
  baseCommit: Type.String(),
  branch: Type.String(),
  headCommit: Type.String(),
}, { additionalProperties: false });

const readChildResultSchema = Type.Object({
  kind: Type.Literal("read"),
  workerType: Type.String(),
  sessionId: Type.String(),
  output: Type.String(),
}, { additionalProperties: false });

const writeChildResultSchema = Type.Object({
  kind: Type.Literal("write"),
  workerType: Type.String(),
  sessionId: Type.String(),
  repositories: Type.Array(repositoryHandoffSchema),
  summary: Type.String(),
  verification: Type.Array(Type.String()),
}, { additionalProperties: false });

export const childResultSchema = Type.Union([readChildResultSchema, writeChildResultSchema]);
