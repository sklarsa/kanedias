import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Value } from "typebox/value";
import { prepareFork, validateForkSource } from "./fork.ts";
import type { ForkSourceSnapshot } from "./fork.ts";
import { verifyHandoff } from "./git-handoff.ts";
import { delegateSessionSchema, handoffSchema } from "./schemas.ts";
import { SupervisorClient } from "./supervisor-client.ts";
import type { CreateChildRequest, DelegateSessionInput, HandoffInput, WorkerSummary } from "./types.ts";

const MAX_TOOL_TEXT = 64 * 1024;
const WORKERS_RETRY_ATTEMPTS = 40;
const WORKERS_RETRY_DELAY_MS = 300;

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
  // E2E controlled question — presented DETERMINISTICALLY on supervisor command,
  // not via the timing-sensitive session_start event. The supervisor drives it
  // by sending a "prompt /present_e2e_question" RPC after the root binds; pi
  // executes this registered slash command (prompt() runs a leading-/ as an
  // extension command), which presents the ui.input question. This removes the
  // fragile dependency on pi's headless-RPC session_start emission timing.
  pi.registerCommand("present_e2e_question", {
    description: "Present the E2E controlled question and await the operator answer.",
    handler: (args, ctx) => {
      const title = (args.trim() || `Kanedias E2E controlled question ${env.KANEDIAS_E2E_RUN_ID ?? ""}`).trim();
      const configuredTimeout = Number(env.KANEDIAS_E2E_QUESTION_TIMEOUT_MS ?? "60000");
      const timeout = Number.isFinite(configuredTimeout) && configuredTimeout > 0 && configuredTimeout <= 60_000 ? configuredTimeout : 60_000;
      // Present the question and await the answer on a detached promise, so the
      // dialog outlives the completing prompt/command and is not cancelled when
      // the invoking RPC command finishes. The answer still routes back.
      void (async () => {
        const answer = await ctx.ui.input(title, "deterministic answer", { timeout });
        ctx.ui.notify(`KANEDIAS_E2E_QUESTION_ANSWER:${answer ?? "cancelled"}`, "info");
      })();
      return;
    },
  });

  // Fetch the configured worker catalog. Retry for a while (the supervisor
  // socket can take a moment to start serving) and NEVER throw: a rejection here
  // aborts the whole extension load and loses every registration (including the
  // E2E controlled-question handler above). If it ultimately fails the extension
  // still loads and delegate_session degrades to an empty catalog.
  let configuredWorkers: WorkerSummary[] = [];
  try {
    for (let attempt = 0; attempt < WORKERS_RETRY_ATTEMPTS; attempt++) {
      try {
        configuredWorkers = await client.workers();
        break;
      } catch (err) {
        if (attempt === WORKERS_RETRY_ATTEMPTS - 1) throw err;
        await new Promise((resolve) => setTimeout(resolve, WORKERS_RETRY_DELAY_MS));
      }
    }
  } catch {
    // Ignore: keep the extension loaded even if the supervisor is unavailable.
  }
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
