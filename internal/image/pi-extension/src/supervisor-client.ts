import { lstat } from "node:fs/promises";
import http from "node:http";
import { Value } from "typebox/value";
import { childResultSchema } from "./schemas.ts";
import type { ChildResult, CreateChildRequest, RepositoryHandoff, WorkerSummary } from "./types.ts";

const MAX_RESPONSE_BYTES = 1024 * 1024;
const DEFAULT_BOUNDED_TIMEOUT_MS = 10_000;

interface SocketStat {
  isSocket(): boolean;
  mode: number;
  uid: number;
}

interface ClientOptions {
  boundedTimeoutMs?: number;
  stat?: (path: string) => Promise<SocketStat>;
}

export class SupervisorClient {
  readonly #socketPath: string;
  readonly #boundedTimeoutMs: number;
  readonly #stat: (path: string) => Promise<SocketStat>;

  constructor(socketPath = "/run/kanedias/supervisor.sock", options: ClientOptions = {}) {
    this.#socketPath = socketPath;
    this.#boundedTimeoutMs = options.boundedTimeoutMs ?? DEFAULT_BOUNDED_TIMEOUT_MS;
    this.#stat = options.stat ?? lstat;
  }

  async workers(signal?: AbortSignal): Promise<WorkerSummary[]> {
    const workers = await this.#request("GET", "/v1/workers", undefined, signal, this.#boundedTimeoutMs);
    if (!Array.isArray(workers)) throw new Error("supervisor returned an invalid worker catalog");
    return workers.map((value) => {
      const worker = value as WorkerSummary;
      if (!worker || typeof worker.workerType !== "string" || typeof worker.description !== "string" ||
          typeof worker.profile?.provider !== "string" || typeof worker.profile.model !== "string") {
        throw new Error("supervisor returned an invalid worker catalog");
      }
      return {
        workerType: worker.workerType,
        description: worker.description,
        profile: {
          provider: worker.profile.provider,
          model: worker.profile.model,
          ...(typeof worker.profile.thinkingLevel === "string" ? { thinkingLevel: worker.profile.thinkingLevel } : {}),
        },
      };
    });
  }

  async createChild(sessionId: string, request: CreateChildRequest, signal?: AbortSignal): Promise<ChildResult> {
    const result = await this.#request("POST", `/v1/sessions/${encodeURIComponent(sessionId)}/children`, request, signal);
    if (!Value.Check(childResultSchema, result)) throw new Error("supervisor returned an invalid child result");
    return result as ChildResult;
  }

  async handoff(input: { repositories: RepositoryHandoff[]; summary: string; verification: string[] }, signal?: AbortSignal): Promise<unknown> {
    return await this.#request("POST", "/v1/handoff", input, signal, this.#boundedTimeoutMs);
  }

  async #request(method: string, requestPath: string, body: unknown, signal?: AbortSignal, timeoutMs?: number): Promise<unknown> {
    const info = await this.#stat(this.#socketPath);
    if (!info.isSocket()) throw new Error(`supervisor endpoint is not a Unix socket: ${this.#socketPath}`);
    if ((info.mode & 0o777) !== 0o600) throw new Error(`supervisor Unix socket must have mode 0600: ${this.#socketPath}`);
    if (typeof process.geteuid === "function" && info.uid !== process.geteuid()) {
      throw new Error(`supervisor Unix socket owner uid must match effective uid: ${this.#socketPath}`);
    }

    const timeout = timeoutMs === undefined ? undefined : AbortSignal.timeout(timeoutMs);
    const combinedSignal = signal && timeout ? AbortSignal.any([signal, timeout]) : signal ?? timeout;
    const encoded = body === undefined ? undefined : Buffer.from(JSON.stringify(body));

    try {
      return await new Promise<unknown>((resolve, reject) => {
        const request = http.request({
          socketPath: this.#socketPath,
          method,
          path: requestPath,
          headers: {
            "content-type": "application/json",
            "accept": "application/json",
            ...(encoded ? { "content-length": String(encoded.length) } : {}),
          },
          signal: combinedSignal,
        }, (response) => {
          const contentType = response.headers["content-type"] ?? "";
          if (!/^application\/json(?:\s*;|$)/i.test(contentType)) {
            response.resume();
            reject(new Error(`supervisor response requires application/json content-type, got ${contentType || "none"}`));
            return;
          }
          const chunks: Buffer[] = [];
          let size = 0;
          response.on("data", (chunk: Buffer) => {
            size += chunk.length;
            if (size > MAX_RESPONSE_BYTES) {
              request.destroy(new Error("supervisor response exceeds 1 MiB"));
              return;
            }
            chunks.push(chunk);
          });
          response.on("end", () => {
            if (size > MAX_RESPONSE_BYTES) return;
            const text = Buffer.concat(chunks).toString("utf8");
            let parsed: unknown;
            try { parsed = text.length === 0 ? {} : JSON.parse(text); }
            catch { reject(new Error("supervisor returned malformed JSON")); return; }
            if ((response.statusCode ?? 500) < 200 || (response.statusCode ?? 500) >= 300) {
              reject(new Error(`supervisor request failed with HTTP ${response.statusCode}: ${text.slice(0, 1024)}`));
              return;
            }
            resolve(parsed);
          });
        });
        request.on("error", reject);
        if (encoded) request.write(encoded);
        request.end();
      });
    } catch (error) {
      if (timeout?.aborted && !signal?.aborted) throw new Error(`supervisor request timed out after ${timeoutMs}ms`, { cause: error });
      if (signal?.aborted) throw new Error("supervisor request cancelled", { cause: error });
      throw error;
    }
  }
}
