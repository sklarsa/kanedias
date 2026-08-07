import { realpath } from "node:fs/promises";
import path from "node:path";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import type { HandoffInput, RepositoryHandoff } from "./types.ts";

const DEFAULT_WORKSPACE_ROOT = "/workspace/repos";

export interface DurableHandoff {
  repositories: RepositoryHandoff[];
  summary: string;
  verification: string[];
}

interface VerificationOptions {
  workspaceRoot?: string;
  signal?: AbortSignal;
}

export function durableHandoff(input: HandoffInput): DurableHandoff {
  return {
    repositories: input.repositories.map(({ path: _path, ...repository }) => repository),
    summary: input.summary,
    verification: [...input.verification],
  };
}

async function git(pi: Pick<ExtensionAPI, "exec">, args: string[], signal: AbortSignal | undefined, description: string): Promise<string> {
  const result = await pi.exec("git", args, signal ? { signal } : {});
  if (result.code !== 0) {
    const detail = result.stderr.trim() || result.stdout.trim() || `exit code ${result.code}`;
    throw new Error(`${description}: ${detail}`);
  }
  return result.stdout.trim();
}

function repositorySlug(remote: string): string | undefined {
  let remotePath = remote.trim();
  const scp = /^[^/\s@]+@[^/\s:]+:(.+)$/.exec(remotePath);
  if (scp) {
    remotePath = scp[1]!;
  } else {
    try {
      const parsed = new URL(remotePath);
      remotePath = decodeURIComponent(parsed.pathname);
    } catch {
      // Local path remotes are valid for verification and tests.
    }
  }
  const segments = remotePath.replace(/\\/g, "/").split("/").filter(Boolean);
  if (segments.length < 2) return undefined;
  const name = segments.at(-1)!.replace(/\.git$/, "");
  const owner = segments.at(-2)!;
  if (!owner || !name) return undefined;
  return `${owner}/${name}`;
}

function isContained(root: string, checkout: string): boolean {
  const relative = path.relative(root, checkout);
  return relative !== "" && relative !== ".." && !relative.startsWith(`..${path.sep}`) && !path.isAbsolute(relative);
}

export async function verifyHandoff(
  pi: Pick<ExtensionAPI, "exec">,
  input: HandoffInput,
  options: VerificationOptions = {},
): Promise<DurableHandoff> {
  const workspaceRoot = await realpath(options.workspaceRoot ?? DEFAULT_WORKSPACE_ROOT);
  const seen = new Set<string>();

  for (const repository of input.repositories) {
    if (seen.has(repository.repository)) throw new Error(`duplicate repository handoff: ${repository.repository}`);
    seen.add(repository.repository);

    if (!path.isAbsolute(repository.path)) throw new Error(`repository checkout path must be absolute: ${repository.path}`);
    const lexicalPath = path.resolve(repository.path);
    const checkout = await realpath(repository.path);
    if (checkout !== lexicalPath) throw new Error(`repository checkout path must not use symlinks: ${repository.path}`);
    if (!isContained(workspaceRoot, checkout)) throw new Error(`repository checkout is outside workspace root ${workspaceRoot}: ${repository.path}`);

    // Keep this order synchronized with the security contract and tests.
    const topLevel = await git(pi, ["-C", checkout, "rev-parse", "--show-toplevel"], options.signal, "resolve repository top level");
    if (await realpath(topLevel) !== checkout) throw new Error(`repository path is not its Git top level: ${repository.path}`);

    const localHead = await git(pi, ["-C", checkout, "rev-parse", "HEAD"], options.signal, "resolve local HEAD");
    if (localHead !== repository.headCommit) {
      throw new Error(`local HEAD mismatch for ${repository.repository}: expected ${repository.headCommit}, got ${localHead}`);
    }

    await git(pi, ["-C", checkout, "check-ref-format", "--branch", repository.branch], options.signal, `invalid branch ${repository.branch}`);

    const origin = await git(pi, ["-C", checkout, "remote", "get-url", "origin"], options.signal, "resolve origin remote");
    const originSlug = repositorySlug(origin);
    if (originSlug !== repository.repository) {
      throw new Error(`repository slug mismatch: handoff names ${repository.repository}, origin resolves to ${originSlug ?? "an unknown repository"}`);
    }

    const remoteRef = `refs/heads/${repository.branch}`;
    const remote = await git(pi, ["-C", checkout, "ls-remote", "--exit-code", "origin", remoteRef], options.signal, `remote branch ${remoteRef} is absent`);
    const matches = remote.split("\n").map((line) => line.trim().split(/\s+/, 2)).filter((parts) => parts.length === 2 && parts[1] === remoteRef);
    if (matches.length !== 1 || matches[0]![0] !== repository.headCommit) {
      throw new Error(`remote branch tip mismatch for ${repository.repository}:${repository.branch}: expected ${repository.headCommit}, got ${matches[0]?.[0] ?? "missing"}`);
    }
  }

  return durableHandoff(input);
}
