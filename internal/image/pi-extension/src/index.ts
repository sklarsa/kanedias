import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { prepareFork, validateForkSource } from "./fork.ts";
import type { ForkSourceSnapshot } from "./fork.ts";
import { verifyHandoff } from "./git-handoff.ts";
import { delegateSessionSchema, handoffSchema } from "./schemas.ts";
import { SupervisorClient } from "./supervisor-client.ts";
import type { CreateChildRequest, DelegateSessionInput, HandoffInput } from "./types.ts";

const MAX_TOOL_TEXT = 64 * 1024;

interface ExtensionOptions {
  env?: Record<string, string | undefined>;
  workspaceRoot?: string;
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

export default async function kanediasExtension(pi: ExtensionAPI, options: ExtensionOptions = {}): Promise<void> {
  const env = options.env ?? process.env;
  const client = new SupervisorClient(env.KANEDIAS_SUPERVISOR_SOCKET ?? "/run/kanedias/supervisor.sock");

  // Register the E2E controlled-question handler before the supervisor
  // /v1/workers call below, which can block or fail if pi boots before the
  // supervisor socket is serving. If registration happened after that await and
  // it was slow, the handler would miss the session_start (startup) event and
  // the E2E controlled question would never surface.
  if (env.KANEDIAS_E2E_RUN_ID && env.KANEDIAS_SESSION_KIND === "root") {
    pi.on("session_start", (event, ctx) => {
      if (event.reason !== "startup") return;
      // Do not block extension startup: RPC input must be live before the
      // controlled dialog can receive its routed response.
      setTimeout(async () => {
        const configuredTimeout = Number(env.KANEDIAS_E2E_QUESTION_TIMEOUT_MS ?? "60000");
        const timeout = Number.isFinite(configuredTimeout) && configuredTimeout > 0 && configuredTimeout <= 60_000 ? configuredTimeout : 60_000;
        const answer = await ctx.ui.input(`Kanedias E2E controlled question ${env.KANEDIAS_E2E_RUN_ID}`, "deterministic answer", { timeout });
        ctx.ui.notify(`KANEDIAS_E2E_QUESTION_ANSWER:${answer ?? "cancelled"}`, "info");
      }, 0);
    });
  }

  const configuredWorkers = await client.workers();
  const workerDescription = configuredWorkers
    .map((worker) => `${worker.workerType}: ${worker.description}`)
    .join("; ");

  pi.registerTool({
    name: "delegate_session",
    label: "Delegate Session",
    description: `Synchronously run a task in a supervised read or write child session using fresh or forked context. Configured workers: ${workerDescription}`,
    promptSnippet: "Delegate an independent task to a supervised child session",
    promptGuidelines: ["Use delegate_session only when the task is independent enough to justify a separate supervised session."],
    parameters: delegateSessionSchema,
    async execute(_toolCallId, params, signal, _onUpdate, ctx) {
      if (!Value.Check(delegateSessionSchema, params)) throw new Error("invalid delegate_session arguments");
      const input = params as DelegateSessionInput;
      let forkSource: ForkSourceSnapshot | undefined;
      if (input.context === "fork") {
        const sessionFile = env.KANEDIAS_PI_SESSION_FILE || ctx.sessionManager.getSessionFile();
        const leafEntryId = ctx.sessionManager.getLeafId();
        if (!sessionFile || !leafEntryId) throw new Error("fork requires a persisted current session and leaf");
        forkSource = await validateForkSource(sessionFile, leafEntryId);
      }

      const worker = configuredWorkers.find((candidate) => candidate.workerType === input.workerType);
      if (!worker) throw new Error(`unknown worker type: ${input.workerType}`);

      const request: CreateChildRequest = { ...input };
      if (forkSource) {
        request.fork = await prepareFork(forkSource, worker.profile);
      }

      const result = await client.createChild(requiredEnvironment(env, "KANEDIAS_SESSION_ID"), request, signal);
      const text = result.kind === "read"
        ? result.output
        : [
            "Repositories:",
            ...result.repositories.map((repository) =>
              `${repository.repository} base=${repository.baseCommit} branch=${repository.branch} head=${repository.headCommit}`),
            "",
            "Summary:",
            result.summary,
            "",
            "Verification:",
            ...result.verification,
          ].join("\n");
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
      const durable = await verifyHandoff(pi, input, {
        ...(signal ? { signal } : {}),
        ...(options.workspaceRoot ? { workspaceRoot: options.workspaceRoot } : {}),
      });
      const acceptance = await client.handoff(durable, signal);
      const ownSessionID = requiredEnvironment(env, "KANEDIAS_SESSION_ID");
      if (!acceptance || acceptance.accepted !== true || acceptance.sessionId !== ownSessionID) {
        throw new Error("supervisor returned an invalid handoff acceptance");
      }
      ctx.shutdown();
      return {
        content: [{ type: "text" as const, text: "Handoff accepted. Shutting down the writer session." }],
        details: durable,
        terminate: true,
      };
    },
  });
}
