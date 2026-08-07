import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { prepareFork, validateForkSource } from "./fork.ts";
import type { ForkSourceSnapshot } from "./fork.ts";
import { durableHandoff } from "./git-handoff.ts";
import { delegateSessionSchema, handoffSchema } from "./schemas.ts";
import { SupervisorClient } from "./supervisor-client.ts";
import type { CreateChildRequest, DelegateSessionInput, HandoffInput } from "./types.ts";

const MAX_TOOL_TEXT = 64 * 1024;

interface ExtensionOptions {
  env?: Record<string, string | undefined>;
}

function requiredEnvironment(env: Record<string, string | undefined>, name: string): string {
  const value = env[name];
  if (!value) throw new Error(`${name} is required in a supervised session`);
  return value;
}

function boundedText(text: string): string {
  if (Buffer.byteLength(text) <= MAX_TOOL_TEXT) return text;
  return `${Buffer.from(text).subarray(0, MAX_TOOL_TEXT - 64).toString("utf8")}\n[output truncated by Kanedias]`;
}

export default function kanediasExtension(pi: ExtensionAPI, options: ExtensionOptions = {}): void {
  const env = options.env ?? process.env;
  const client = new SupervisorClient(env.KANEDIAS_SUPERVISOR_SOCKET ?? "/run/kanedias/supervisor.sock");

  pi.registerTool({
    name: "delegate_session",
    label: "Delegate Session",
    description: "Synchronously run a task in a supervised read or write child session using fresh or forked context.",
    promptSnippet: "Delegate an independent task to a supervised child session",
    promptGuidelines: ["Use delegate_session only when the task is independent enough to justify a separate supervised session."],
    parameters: delegateSessionSchema,
    async execute(_toolCallId, params, signal, _onUpdate, ctx) {
      if (!Value.Check(delegateSessionSchema, params)) throw new Error("invalid delegate_session arguments");
      const input = params as DelegateSessionInput;
      let forkSource: ForkSourceSnapshot | undefined;
      if (input.context === "fork") {
        const sessionFile = env.KANEDIAS_PI_SESSION_FILE ?? ctx.sessionManager.getSessionFile();
        const leafEntryId = ctx.sessionManager.getLeafId();
        if (!sessionFile || !leafEntryId) throw new Error("fork requires a persisted current session and leaf");
        forkSource = await validateForkSource(sessionFile, leafEntryId);
      }

      const workers = await client.workers(signal);
      const worker = workers.find((candidate) => candidate.workerType === input.workerType);
      if (!worker) throw new Error(`unknown worker type: ${input.workerType}`);

      const request: CreateChildRequest = { ...input };
      if (forkSource) {
        request.fork = await prepareFork(forkSource, worker.profile);
      }

      const result = await client.createChild(requiredEnvironment(env, "KANEDIAS_SESSION_ID"), request, signal);
      const text = result.kind === "read" ? result.output : `${result.summary}\n\nVerification:\n${result.verification.join("\n")}`;
      return { content: [{ type: "text" as const, text: boundedText(text) }], details: result };
    },
  });

  pi.registerTool({
    name: "handoff",
    label: "Writer Handoff",
    description: "Submit exact durable Git refs and terminal verification from a supervised write session.",
    promptSnippet: "Finish a write session with durable Git refs",
    promptGuidelines: ["Call handoff alone in the final assistant tool batch after commits and remote refs are ready."],
    parameters: handoffSchema,
    executionMode: "sequential",
    async execute(_toolCallId, params, signal, _onUpdate, ctx: ExtensionContext) {
      if (env.KANEDIAS_SESSION_KIND !== "write") throw new Error("handoff is available only in a supervised write session");
      if (!Value.Check(handoffSchema, params)) throw new Error("invalid handoff arguments");
      const input = params as HandoffInput;
      await client.handoff(durableHandoff(input), signal);
      ctx.shutdown();
      return {
        content: [{ type: "text" as const, text: "Handoff accepted. Shutting down the writer session." }],
        details: durableHandoff(input),
        terminate: true,
      };
    },
  });
}
