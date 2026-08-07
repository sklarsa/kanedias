import { lstat, realpath } from "node:fs/promises";
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
  let remotePath: string;
  const value = remote.trim();
  const scp = /^git@github\.com:(.+)$/.exec(value);
  if (scp) {
    remotePath = scp[1]!;
  } else {
    try {
      const parsed = new URL(value);
      if (parsed.hostname.toLowerCase() !== "github.com") return undefined;
      if (parsed.protocol !== "https:" && parsed.protocol !== "ssh:") return undefined;
      if (parsed.protocol === "ssh:" && parsed.username !== "git") return undefined;
      remotePath = decodeURIComponent(parsed.pathname);
    } catch {
      return undefined;
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

interface DirectoryIdentity {
  canonicalPath: string;
  dev: number;
  ino: number;
}

async function trustedWorkspaceRoot(input: string): Promise<string> {
  const normalized = path.resolve(input);
  try {
    if (input !== normalized) throw new Error("is not a normalized absolute path");
    const info = await lstat(normalized);
    if (info.isSymbolicLink() || !info.isDirectory()) throw new Error("not a non-symlink directory");
    const canonical = await realpath(normalized);
    if (canonical !== normalized) throw new Error("does not resolve to its normalized absolute path");
    return canonical;
  } catch (error) {
    throw new Error(`workspace root is not a trusted literal directory: ${input}`, { cause: error });
  }
}

async function captureCheckout(root: string, input: string): Promise<DirectoryIdentity> {
  if (!path.isAbsolute(input)) throw new Error(`repository checkout path must be absolute: ${input}`);
  const normalized = path.resolve(input);
  const info = await lstat(normalized);
  if (info.isSymbolicLink() || !info.isDirectory()) throw new Error(`repository checkout path must be a non-symlink directory: ${input}`);
  const canonicalPath = await realpath(normalized);
  if (canonicalPath !== normalized) throw new Error(`repository checkout path must not use symlinks: ${input}`);
  if (!isContained(root, canonicalPath)) throw new Error(`repository checkout is outside workspace root ${root}: ${input}`);
  return { canonicalPath, dev: info.dev, ino: info.ino };
}

async function revalidateCheckout(root: string, input: string, expected: DirectoryIdentity): Promise<void> {
  let current: DirectoryIdentity;
  try {
    current = await captureCheckout(root, input);
  } catch (error) {
    throw new Error(`repository checkout identity changed during verification: ${input}`, { cause: error });
  }
  if (current.canonicalPath !== expected.canonicalPath || current.dev !== expected.dev || current.ino !== expected.ino) {
    throw new Error(`repository checkout identity changed during verification: ${input}`);
  }
}

export async function verifyHandoff(
  pi: Pick<ExtensionAPI, "exec">,
  input: HandoffInput,
  options: VerificationOptions = {},
): Promise<DurableHandoff> {
  const workspaceRoot = await trustedWorkspaceRoot(options.workspaceRoot ?? DEFAULT_WORKSPACE_ROOT);
  const seen = new Set<string>();

  for (const repository of input.repositories) {
    if (seen.has(repository.repository)) throw new Error(`duplicate repository handoff: ${repository.repository}`);
    seen.add(repository.repository);

    const identity = await captureCheckout(workspaceRoot, repository.path);
    const checkout = identity.canonicalPath;
    const checkedGit = async (args: string[], description: string): Promise<string> => {
      await revalidateCheckout(workspaceRoot, repository.path, identity);
      try {
        return await git(pi, args, options.signal, description);
      } finally {
        await revalidateCheckout(workspaceRoot, repository.path, identity);
      }
    };

    // Keep this order synchronized with the security contract and tests.
    const topLevel = await checkedGit(["-C", checkout, "rev-parse", "--show-toplevel"], "resolve repository top level");
    if (await realpath(topLevel) !== checkout) throw new Error(`repository path is not its Git top level: ${repository.path}`);

    const localHead = await checkedGit(["-C", checkout, "rev-parse", "HEAD"], "resolve local HEAD");
    if (localHead !== repository.headCommit) {
      throw new Error(`local HEAD mismatch for ${repository.repository}: expected ${repository.headCommit}, got ${localHead}`);
    }

    await checkedGit(["-C", checkout, "check-ref-format", "--branch", repository.branch], `invalid branch ${repository.branch}`);

    const origin = await checkedGit(["-C", checkout, "config", "--get", "remote.origin.url"], "resolve literal origin remote");
    const originSlug = repositorySlug(origin);
    if (originSlug !== repository.repository) {
      throw new Error(`repository slug mismatch: handoff names ${repository.repository}, origin resolves to ${originSlug ?? "an unknown repository"}`);
    }

    const remoteRef = `refs/heads/${repository.branch}`;
    const remote = await checkedGit(["-C", checkout, "ls-remote", "--exit-code", "origin", remoteRef], `remote branch ${remoteRef} is absent`);
    const matches = remote.split("\n").map((line) => line.trim().split(/\s+/, 2)).filter((parts) => parts.length === 2 && parts[1] === remoteRef);
    if (matches.length !== 1 || matches[0]![0] !== repository.headCommit) {
      throw new Error(`remote branch tip mismatch for ${repository.repository}:${repository.branch}: expected ${repository.headCommit}, got ${matches[0]?.[0] ?? "missing"}`);
    }
    await revalidateCheckout(workspaceRoot, repository.path, identity);
  }

  return durableHandoff(input);
}
