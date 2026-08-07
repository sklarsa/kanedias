import { readFile, writeFile } from "node:fs/promises";
import { SessionManager } from "@earendil-works/pi-coding-agent";
import type { ForkSpec, ModelProfile } from "./types.ts";

interface JsonObject { [key: string]: unknown }

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
    return [block];
  });
}

export async function validateForkSource(sessionFile: string, leafEntryId: string): Promise<void> {
  if (!sessionFile.trim()) throw new Error("fork requires a persisted source session");
  await readFile(sessionFile);
  const manager = SessionManager.open(sessionFile);
  if (!manager.isPersisted()) throw new Error("fork requires a persisted source session");
  if (!manager.getEntry(leafEntryId)) throw new Error(`fork leaf is not persisted: ${leafEntryId}`);
}

export async function prepareFork(sessionFile: string, leafEntryId: string, target: ModelProfile): Promise<ForkSpec> {
  await validateForkSource(sessionFile, leafEntryId);
  const parentBefore = await readFile(sessionFile);
  const manager = SessionManager.open(sessionFile);
  const branchPath = manager.createBranchedSession(leafEntryId);
  if (!branchPath) throw new Error("failed to persist forked session");
  const branchManager = SessionManager.open(branchPath);
  if (branchManager.getLeafId() !== leafEntryId) throw new Error("forked session does not end at requested leaf");

  const branchText = await readFile(branchPath, "utf8");
  const lines = branchText.trimEnd().split("\n").map((line) => JSON.parse(line) as JsonObject);
  for (const line of lines) sanitizeSignedThinking(line, target);
  await writeFile(branchPath, `${lines.map((line) => JSON.stringify(line)).join("\n")}\n`, { mode: 0o600 });

  const parentAfter = await readFile(sessionFile);
  if (!parentAfter.equals(parentBefore)) throw new Error("fork preparation modified the parent session");

  return { sessionFile: branchPath, piSessionId: branchManager.getSessionId(), leafEntryId };
}
