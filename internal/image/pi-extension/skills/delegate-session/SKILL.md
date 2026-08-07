---
name: delegate-session
description: Use when a task may benefit from an independent supervised Pi session with its own sandbox and transcript.
---

# Delegate Session

## Overview

Delegate only work that is independent enough to justify a synchronous child session. The caller waits for the child result; delegation is not a background job.

## Choose the Request

| Field | Choice |
|---|---|
| `workerType` | Select a configured worker whose description matches the task. Do not invent worker names. |
| `kind` | `read` for analysis/review with no durable changes; `write` for changes that must return Git refs. |
| `context` | `fresh` for a clean transcript; `fork` when the child needs the current persisted conversation branch. |
| `task` | Give one bounded, self-contained deliverable and its required evidence. |

A fork copies the current transcript path into a new Pi session identity. It does not move or modify the parent session. Provider-incompatible signed thinking is removed while ordinary messages and tool history remain.

## Workflow

1. Decide whether delegation saves time or provides useful isolation.
2. Pick the worker, kind, and context deliberately.
3. Call `delegate_session` with a precise task.
4. Wait: the call completes only when a read child returns its answer or a write child submits an accepted handoff.
5. Inspect the returned typed details and incorporate the result.

## When Not to Delegate

Do the work locally when it is small, tightly coupled to your next edit, needs constant back-and-forth, or cannot be described as an independently verifiable task. Do not delegate merely to avoid reading available context. Do not use a write child for review-only work.

## Common Mistakes

- Treating the call as detached work: it is synchronous.
- Using `fresh` while assuming the child saw this conversation.
- Using `fork` before the current leaf is persisted.
- Delegating overlapping writes without explicit ownership boundaries.
- Ignoring a write result's exact commits and verification evidence.
