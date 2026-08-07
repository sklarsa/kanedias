import { randomUUID } from "node:crypto";
import { readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { SessionManager } from "@earendil-works/pi-coding-agent";
import type { ForkSpec, ModelProfile } from "./types.ts";

interface JsonObject { [key: string]: unknown }

export interface ForkSourceSnapshot {
  sessionFile: string;
  leafEntryId: string;
  bytes: Buffer;
}

function parseStrictJsonLines(bytes: Buffer, sessionFile: string): JsonObject[] {
  let text: string;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch {
    throw new Error(`fork source is not valid UTF-8: ${sessionFile}`);
  }

  const entries: JsonObject[] = [];
  for (const [index, line] of text.split("\n").entries()) {
    if (!line.trim()) continue;
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch {
      throw new Error(`fork source contains malformed JSONL at line ${index + 1}: ${sessionFile}`);
    }
    if (typeof value !== "object" || value === null || Array.isArray(value)) {
      throw new Error(`fork source contains an invalid JSONL entry at line ${index + 1}: ${sessionFile}`);
    }
    entries.push(value as JsonObject);
  }

  if (entries.length === 0) throw new Error("fork requires a persisted source session");
  const header = entries[0]!;
  if (header.type !== "session" || typeof header.id !== "string" || !header.id ||
      typeof header.timestamp !== "string" || typeof header.cwd !== "string") {
    throw new Error("fork requires a persisted source session header");
  }
  if (entries.slice(1).some((entry) => entry.type === "session")) {
    throw new Error("fork source contains multiple session headers");
  }
  return entries;
}

function validateSelectedPath(entries: JsonObject[], leafEntryId: string): void {
  const byId = new Map<string, JsonObject>();
  for (const entry of entries.slice(1)) {
    if (typeof entry.id !== "string" || !entry.id || (entry.parentId !== null && typeof entry.parentId !== "string")) {
      throw new Error("fork source contains an invalid persisted entry");
    }
    if (byId.has(entry.id)) throw new Error(`fork source contains duplicate entry id: ${entry.id}`);
    byId.set(entry.id, entry);
  }
  if (!byId.has(leafEntryId)) throw new Error(`fork leaf is not persisted: ${leafEntryId}`);

  const visited = new Set<string>();
  let current: string | null = leafEntryId;
  while (current !== null) {
    if (visited.has(current)) throw new Error("fork source selected path contains a cycle");
    visited.add(current);
    const entry = byId.get(current);
    if (!entry) throw new Error(`fork source selected path has a missing parent: ${current}`);
    current = entry.parentId as string | null;
  }
}

function sanitizeSignedThinking(line: JsonObject, target: ModelProfile): void {
  if (line.type !== "message" || typeof line.message !== "object" || line.message === null) return;
  const message = line.message as JsonObject;
  if (message.role !== "assistant" || !Array.isArray(message.content)) return;
  const compatible = message.provider === target.provider && message.model === target.model;
  if (compatible) return;
  message.content = message.content.flatMap((value) => {
    if (typeof value !== "object" || value === null) return [value];
    const block = value as JsonObject;
    if (block.type === "thinking" && typeof block.thinkingSignature === "string") return [];
    if (block.type === "toolCall" && "thoughtSignature" in block) {
      const { thoughtSignature: _signature, ...unsigned } = block;
      return [unsigned];
    }
    if (block.type === "text" && "textSignature" in block) {
      const { textSignature: _signature, ...unsigned } = block;
      return [unsigned];
    }
    return [block];
  });
}

export async function validateForkSource(sessionFile: string, leafEntryId: string): Promise<ForkSourceSnapshot> {
  if (!sessionFile.trim()) throw new Error("fork requires a persisted source session");
  const bytes = await readFile(sessionFile);
  const entries = parseStrictJsonLines(bytes, sessionFile);
  validateSelectedPath(entries, leafEntryId);
  return { sessionFile, leafEntryId, bytes };
}

export async function prepareFork(sessionFile: string, leafEntryId: string, target: ModelProfile): Promise<ForkSpec>;
export async function prepareFork(source: ForkSourceSnapshot, target: ModelProfile): Promise<ForkSpec>;
export async function prepareFork(
  sourceOrFile: ForkSourceSnapshot | string,
  leafOrTarget: string | ModelProfile,
  maybeTarget?: ModelProfile,
): Promise<ForkSpec> {
  const source = typeof sourceOrFile === "string"
    ? await validateForkSource(sourceOrFile, leafOrTarget as string)
    : sourceOrFile;
  const target = (typeof sourceOrFile === "string" ? maybeTarget : leafOrTarget) as ModelProfile;
  const stagingPath = path.join(path.dirname(source.sessionFile), `.${path.basename(source.sessionFile)}.kanedias-${randomUUID()}.jsonl`);
  let branchPath: string | undefined;
  let completed = false;

  try {
    const currentParent = await readFile(source.sessionFile);
    if (!currentParent.equals(source.bytes)) throw new Error("fork source changed after validation");

    await writeFile(stagingPath, source.bytes, { flag: "wx", mode: 0o600 });
    const manager = SessionManager.open(stagingPath);
    if (!manager.isPersisted()) throw new Error("fork requires a persisted source session");
    const selectedPath = manager.getBranch(source.leafEntryId);
    if (selectedPath.length === 0) throw new Error(`fork leaf is not persisted: ${source.leafEntryId}`);
    const selectedConversationIds = selectedPath.filter((entry) => entry.type !== "label").map((entry) => entry.id);
    const selectedLabels = selectedConversationIds.flatMap((targetId) => {
      const label = manager.getLabel(targetId);
      return label === undefined ? [] : [{ targetId, label }];
    });

    branchPath = manager.createBranchedSession(source.leafEntryId);
    if (!branchPath) throw new Error("failed to persist forked session");

    const branchEntries = parseStrictJsonLines(await readFile(branchPath), branchPath);
    branchEntries[0]!.parentSession = source.sessionFile;
    for (const entry of branchEntries) sanitizeSignedThinking(entry, target);
    await writeFile(branchPath, `${branchEntries.map((entry) => JSON.stringify(entry)).join("\n")}\n`, { mode: 0o600 });

    const branchManager = SessionManager.open(branchPath);
    const actualLeafEntryId = branchManager.getLeafId();
    if (!actualLeafEntryId) throw new Error("forked session has no persisted leaf");
    const actualConversationIds = branchManager.getBranch(actualLeafEntryId)
      .filter((entry) => entry.type !== "label")
      .map((entry) => entry.id);
    if (JSON.stringify(actualConversationIds) !== JSON.stringify(selectedConversationIds)) {
      throw new Error("forked session does not preserve the selected conversation path");
    }
    for (const { targetId, label } of selectedLabels) {
      if (branchManager.getLabel(targetId) !== label) throw new Error(`forked session does not preserve label metadata for ${targetId}`);
    }

    const parentAfter = await readFile(source.sessionFile);
    if (!parentAfter.equals(source.bytes)) throw new Error("fork preparation modified the parent session");
    completed = true;
    return { sessionFile: branchPath, piSessionId: branchManager.getSessionId(), leafEntryId: actualLeafEntryId };
  } finally {
    await rm(stagingPath, { force: true });
    if (!completed && branchPath) await rm(branchPath, { force: true });
    const parentAfter = await readFile(source.sessionFile);
    if (!parentAfter.equals(source.bytes)) throw new Error("fork preparation modified the parent session");
  }
}
