---
name: writer-handoff
description: Use when completing changes in a supervised write session and returning durable repository results to its parent.
---

# Writer Handoff

## Overview

A writer finishes by returning immutable, remotely reachable Git identities plus verification evidence. The supervisor does not require or manufacture a clean working tree; deciding and documenting the correct commit boundary is your responsibility.

## Required Workflow

For every modified repository:

1. Review the diff and choose an intentional commit boundary.
2. Run the relevant verification and record the exact commands/results.
3. Commit the intended changes. Do not report uncommitted work as delivered.
4. Push the branch to the named repository remote.
5. Resolve and record:
   - absolute local checkout `path` under `/workspace/repos` used for verification (never a symlink);
   - `repository` remote identity;
   - exact `baseCommit` the work builds on;
   - pushed `branch` name;
   - exact `headCommit` at that remote branch.
6. Verify that the remote branch resolves to `headCommit`; include this evidence with test results.

The handoff tool independently verifies each checkout with argument-array Git execution in this order: repository top level, local `HEAD`, branch format, `origin` URL, and exact `ls-remote` branch tip. The checkout must be contained under `/workspace/repos`, its `origin` slug must match `repository`, local `HEAD` must equal `headCommit`, and the remote branch tip must equal that same commit. Local checkout paths are removed before the durable result is sent to the supervisor.

Git cleanliness is discipline, not an automatic handoff gate. Inspect ignored, untracked, and modified files yourself. If unrelated local state remains, do not silently include or discard it; make the delivered commit boundary and residual state explicit.

## Terminal Handoff

Call `handoff` only after every reported ref is pushed and verified. Put `handoff` **alone in the final assistant tool batch**. Do not include sibling tool calls: `terminate: true` is batch-wide only when every sibling result also terminates.

A rejected handoff is non-terminal. Correct the refs or evidence and retry. An accepted handoff is terminal and requests graceful Pi shutdown; do not plan additional work afterward.

## Quick Reference

| Handoff field | Evidence |
|---|---|
| `path` | Absolute checkout used for local verification; it is stripped from the durable parent result. |
| `repository` | The exact intended remote repository, not merely a directory name. |
| `baseCommit` | Immutable commit from which the delivered change was based. |
| `branch` | Existing pushed remote branch. |
| `headCommit` | Immutable commit currently resolved by that branch. |
| `verification` | Exact commands and concise outcomes. |

## Common Mistakes

- Calling handoff before push completes.
- Reporting local `HEAD` without checking the remote ref, or naming a repository that does not match `origin`.
- Passing a symlink, nested directory, or checkout outside `/workspace/repos` as `path`.
- Assuming a clean tree is supervisor-enforced.
- Omitting the base commit or using symbolic names where an exact commit is required.
- Calling another tool beside the terminal handoff.
