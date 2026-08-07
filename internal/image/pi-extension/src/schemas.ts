import { Type } from "typebox";

const nonBlank = Type.String({ minLength: 1, pattern: ".*\\S.*" });

export const delegateSessionSchema = Type.Object({
  workerType: nonBlank,
  kind: Type.Union([Type.Literal("read"), Type.Literal("write")]),
  context: Type.Union([Type.Literal("fresh"), Type.Literal("fork")]),
  task: nonBlank,
}, { additionalProperties: false });

const repositoryHandoffInputSchema = Type.Object({
  path: nonBlank,
  repository: nonBlank,
  baseCommit: nonBlank,
  branch: nonBlank,
  headCommit: nonBlank,
}, { additionalProperties: false });

export const handoffSchema = Type.Object({
  repositories: Type.Array(repositoryHandoffInputSchema, { minItems: 1 }),
  summary: nonBlank,
  verification: Type.Array(nonBlank),
}, { additionalProperties: false });

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
