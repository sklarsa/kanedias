import type { HandoffInput, RepositoryHandoff } from "./types.ts";

export interface DurableHandoff {
  repositories: RepositoryHandoff[];
  summary: string;
  verification: string[];
}

export function durableHandoff(input: HandoffInput): DurableHandoff {
  return {
    repositories: input.repositories.map(({ path: _path, ...repository }) => repository),
    summary: input.summary,
    verification: [...input.verification],
  };
}
