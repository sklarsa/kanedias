import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { cp, mkdtemp, mkdir, rename, rm, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { promisify } from "node:util";
import test from "node:test";
import { verifyHandoff } from "../src/git-handoff.ts";
import type { HandoffInput } from "../src/types.ts";

const run = promisify(execFile);

async function git(args: string[]): Promise<string> {
  const { stdout } = await run("git", args);
  return stdout.trim();
}

async function repository(root: string, slug: string, branch: string) {
  const checkout = path.join(root, "repos", ...slug.split("/"));
  const remote = path.join(root, "remotes", `${slug}.git`);
  await mkdir(path.dirname(checkout), { recursive: true });
  await mkdir(path.dirname(remote), { recursive: true });
  await git(["init", "--bare", remote]);
  await git(["init", checkout]);
  await git(["-C", checkout, "config", "user.email", "test@example.com"]);
  await git(["-C", checkout, "config", "user.name", "Test"]);
  await writeFile(path.join(checkout, "file.txt"), `${slug}\n`);
  await git(["-C", checkout, "add", "file.txt"]);
  await git(["-C", checkout, "commit", "-m", "initial"]);
  await git(["-C", checkout, "switch", "-c", branch]);
  await git(["-C", checkout, "remote", "add", "origin", remote]);
  await git(["-C", checkout, "push", "-u", "origin", branch]);
  return { checkout, remote, head: await git(["-C", checkout, "rev-parse", "HEAD"]) };
}

function inputFor(repo: Awaited<ReturnType<typeof repository>>, slug: string, branch: string): HandoffInput {
  return {
    repositories: [{ path: repo.checkout, repository: slug, baseCommit: repo.head, branch, headCommit: repo.head }],
    summary: "implemented",
    verification: ["tests passed"],
  };
}

function executingPi(calls: string[][]) {
  return {
    async exec(command: string, args: string[]) {
      calls.push([command, ...args]);
      if (command === "git" && args.at(-3) === "config" && args.at(-2) === "--get" && args.at(-1) === "remote.origin.url") {
        const checkout = args[1]!;
        const segments = checkout.split(path.sep).filter(Boolean);
        return { stdout: `https://github.com/${segments.at(-2)}/${segments.at(-1)}.git\n`, stderr: "", code: 0, killed: false };
      }
      try {
        const { stdout, stderr } = await run(command, args);
        return { stdout, stderr, code: 0, killed: false };
      } catch (error: any) {
        return { stdout: error.stdout ?? "", stderr: error.stderr ?? "", code: error.code ?? 1, killed: false };
      }
    },
  } as any;
}

test("verifies exact local and remote refs for multiple repositories in the required command sequence", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "kanedias-handoff-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const first = await repository(root, "owner/first", "feature/one");
  const second = await repository(root, "other/second", "feature/two");
  await writeFile(path.join(first.checkout, "dirty.txt"), "uncommitted state is allowed\n");
  const calls: string[][] = [];
  const durable = await verifyHandoff(executingPi(calls), {
    repositories: [inputFor(first, "owner/first", "feature/one").repositories[0]!, inputFor(second, "other/second", "feature/two").repositories[0]!],
    summary: "two repositories",
    verification: ["npm test"],
  }, { workspaceRoot: path.join(root, "repos") });

  assert.equal(JSON.stringify(durable).includes("path"), false);
  assert.deepEqual(durable.repositories.map((repo) => [repo.repository, repo.branch, repo.headCommit]), [
    ["owner/first", "feature/one", first.head],
    ["other/second", "feature/two", second.head],
  ]);
  const expected = [first, second].flatMap((repo, index) => {
    const branch = index === 0 ? "feature/one" : "feature/two";
    return [
      ["git", "-C", repo.checkout, "rev-parse", "--show-toplevel"],
      ["git", "-C", repo.checkout, "rev-parse", "HEAD"],
      ["git", "-C", repo.checkout, "check-ref-format", "--branch", branch],
      ["git", "-C", repo.checkout, "config", "--get", "remote.origin.url"],
      ["git", "-C", repo.checkout, "ls-remote", "--exit-code", "origin", `refs/heads/${branch}`],
    ];
  });
  assert.deepEqual(calls, expected);
  assert.equal(calls.some((call) => call.includes("status")), false);
});

test("rejects path escape and symlink before invoking git", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "kanedias-handoff-path-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const workspace = path.join(root, "repos");
  await mkdir(workspace);
  const outside = await repository(path.join(root, "outside-root"), "owner/repo", "feature");
  const link = path.join(workspace, "linked");
  await symlink(outside.checkout, link);
  for (const checkout of [outside.checkout, link]) {
    const calls: string[][] = [];
    await assert.rejects(verifyHandoff(executingPi(calls), {
      ...inputFor(outside, "owner/repo", "feature"),
      repositories: [{ ...inputFor(outside, "owner/repo", "feature").repositories[0]!, path: checkout }],
    }, { workspaceRoot: workspace }), /workspace|symlink|outside/i);
    assert.deepEqual(calls, []);
  }
});

test("rejects a symlinked workspace root before invoking git", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "kanedias-handoff-root-link-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const actualRoot = path.join(root, "actual");
  const linkedRoot = path.join(root, "repos");
  await mkdir(actualRoot);
  await symlink(actualRoot, linkedRoot);
  const calls: string[][] = [];
  await assert.rejects(verifyHandoff(executingPi(calls), {
    repositories: [], summary: "done", verification: [],
  }, { workspaceRoot: linkedRoot }), /workspace root.*trusted literal|canonical/i);
  assert.deepEqual(calls, []);
});

test("fails closed when a checkout path is swapped during Git verification", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "kanedias-handoff-swap-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const repo = await repository(root, "owner/repo", "feature");
  const moved = `${repo.checkout}-original`;
  const replacement = `${repo.checkout}-replacement`;
  await cp(repo.checkout, replacement, { recursive: true });
  let swapped = false;
  const calls: string[][] = [];
  const pi = {
    async exec(command: string, args: string[]) {
      calls.push([command, ...args]);
      const result = await executingPi([]).exec(command, args);
      if (!swapped) {
        swapped = true;
        await rename(repo.checkout, moved);
        await rename(replacement, repo.checkout);
      }
      return result;
    },
  } as any;
  await assert.rejects(verifyHandoff(pi, inputFor(repo, "owner/repo", "feature"), {
    workspaceRoot: path.join(root, "repos"),
  }), /changed|symlink|identity/i);
  assert.equal(calls.length, 1);
});

test("rejects repository mismatch, local head mismatch, invalid or absent branch, and remote tip mismatch", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "kanedias-handoff-reject-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const repo = await repository(root, "owner/repo", "feature");
  const workspaceRoot = path.join(root, "repos");
  const base = inputFor(repo, "owner/repo", "feature");
  const wrong = "0".repeat(repo.head.length);
  const cases: Array<[string, HandoffInput]> = [
    ["repository", { ...base, repositories: [{ ...base.repositories[0]!, repository: "attacker/repo" }] }],
    ["HEAD", { ...base, repositories: [{ ...base.repositories[0]!, headCommit: wrong }] }],
    ["branch", { ...base, repositories: [{ ...base.repositories[0]!, branch: "bad branch" }] }],
    ["remote", { ...base, repositories: [{ ...base.repositories[0]!, branch: "missing" }] }],
  ];
  for (const [message, candidate] of cases) {
    await assert.rejects(verifyHandoff(executingPi([]), candidate, { workspaceRoot }), new RegExp(message, "i"));
  }

  await writeFile(path.join(repo.checkout, "file.txt"), "second\n");
  await git(["-C", repo.checkout, "add", "file.txt"]);
  await git(["-C", repo.checkout, "commit", "-m", "second"]);
  const localHead = await git(["-C", repo.checkout, "rev-parse", "HEAD"]);
  await assert.rejects(verifyHandoff(executingPi([]), {
    ...base,
    repositories: [{ ...base.repositories[0]!, headCommit: localHead }],
  }, { workspaceRoot }), /remote.*mismatch|tip/i);
});

test("rejects non-GitHub and unsafe-protocol origins during guest preflight", async (t) => {
  const root = await mkdtemp(path.join(os.tmpdir(), "kanedias-handoff-origin-"));
  t.after(() => rm(root, { recursive: true, force: true }));
  const repo = await repository(root, "owner/repo", "feature");
  for (const origin of ["https://evil.example/owner/repo.git", "http://github.com/owner/repo.git", repo.remote]) {
    const delegate = executingPi([]);
    const pi = {
      async exec(command: string, args: string[]) {
        if (command === "git" && args.at(-3) === "config" && args.at(-2) === "--get" && args.at(-1) === "remote.origin.url") {
          return { stdout: `${origin}\n`, stderr: "", code: 0, killed: false };
        }
        return delegate.exec(command, args);
      },
    } as any;
    await assert.rejects(verifyHandoff(pi, inputFor(repo, "owner/repo", "feature"), { workspaceRoot: path.join(root, "repos") }), /repository slug mismatch|unknown repository/i);
  }
});
