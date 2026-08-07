export type ChildKind = "read" | "write";
export type ContextMode = "fresh" | "fork";

export interface ModelProfile {
  provider: string;
  model: string;
  thinkingLevel?: string;
}

export interface WorkerSummary {
  workerType: string;
  description: string;
  profile: ModelProfile;
}

export interface ForkSpec {
  sessionFile: string;
  piSessionId: string;
  leafEntryId: string;
}

export interface DelegateSessionInput {
  workerType: string;
  kind: ChildKind;
  context: ContextMode;
  task: string;
}

export interface CreateChildRequest extends DelegateSessionInput {
  fork?: ForkSpec;
}

export interface RepositoryHandoffInput {
  path: string;
  repository: string;
  baseCommit: string;
  branch: string;
  headCommit: string;
}

export interface RepositoryHandoff {
  repository: string;
  baseCommit: string;
  branch: string;
  headCommit: string;
}

export interface HandoffInput {
  repositories: RepositoryHandoffInput[];
  summary: string;
  verification: string[];
}

export interface ReadChildResult {
  kind: "read";
  workerType: string;
  sessionId: string;
  output: string;
}

export interface WriteChildResult {
  kind: "write";
  workerType: string;
  sessionId: string;
  repositories: RepositoryHandoff[];
  summary: string;
  verification: string[];
}

export type ChildResult = ReadChildResult | WriteChildResult;
