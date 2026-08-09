package contract

import (
	"strings"

	"github.com/sklarsa/kanedias/internal/config"
)

type ChildKind string

const (
	ChildKindRoot  ChildKind = "root"
	ChildKindRead  ChildKind = "read"
	ChildKindWrite ChildKind = "write"
)

type ContextMode string

const (
	ContextRoot  ContextMode = "root"
	ContextFresh ContextMode = "fresh"
	ContextFork  ContextMode = "fork"
)

type ForkSpec struct {
	SessionFile string `json:"sessionFile"`
	PiSessionID string `json:"piSessionId"`
	LeafEntryID string `json:"leafEntryId"`
}

// ModelProfile is the canonical credential-free model selection carried by
// every session. It is an alias for the single config runtime representation so
// the API never forks the policy shape.
type ModelProfile = config.ModelProfile

type WorkerSummary struct {
	WorkerType  string       `json:"workerType"`
	Description string       `json:"description"`
	Profile     ModelProfile `json:"profile"`
}

type CreateChildRequest struct {
	WorkerType string      `json:"workerType"`
	Kind       ChildKind   `json:"kind"`
	Context    ContextMode `json:"context"`
	Task       string      `json:"task"`
	Fork       *ForkSpec   `json:"fork,omitempty"`
}

func (request CreateChildRequest) Validate() error {
	if strings.TrimSpace(request.WorkerType) == "" {
		return NewError(ErrorInvalidRequest, "workerType is required")
	}
	if request.Kind != ChildKindRead && request.Kind != ChildKindWrite {
		return NewError(ErrorInvalidRequest, "kind must be read or write")
	}
	if request.Context != ContextFresh && request.Context != ContextFork {
		return NewError(ErrorInvalidRequest, "context must be fresh or fork")
	}
	if strings.TrimSpace(request.Task) == "" {
		return NewError(ErrorInvalidRequest, "task is required")
	}
	if request.Context == ContextFresh {
		if request.Fork != nil {
			return NewError(ErrorInvalidRequest, "fork is forbidden for fresh context")
		}
		return nil
	}
	if request.Fork == nil {
		return NewError(ErrorInvalidRequest, "fork is required for fork context")
	}
	if strings.TrimSpace(request.Fork.SessionFile) == "" {
		return NewError(ErrorInvalidRequest, "fork.sessionFile is required")
	}
	if strings.TrimSpace(request.Fork.PiSessionID) == "" {
		return NewError(ErrorInvalidRequest, "fork.piSessionId is required")
	}
	if strings.TrimSpace(request.Fork.LeafEntryID) == "" {
		return NewError(ErrorInvalidRequest, "fork.leafEntryId is required")
	}
	return nil
}

type RepositoryHandoff struct {
	Repository string `json:"repository"`
	BaseCommit string `json:"baseCommit"`
	Branch     string `json:"branch"`
	HeadCommit string `json:"headCommit"`
}

type ReadChildResult struct {
	Kind       ChildKind `json:"kind"`
	WorkerType string    `json:"workerType"`
	SessionID  string    `json:"sessionId"`
	Output     string    `json:"output"`
}

func (result ReadChildResult) Validate() error {
	if result.Kind != ChildKindRead {
		return NewError(ErrorInvalidRequest, "read result kind must be read")
	}
	return nil
}

type WriteChildResult struct {
	Kind         ChildKind           `json:"kind"`
	WorkerType   string              `json:"workerType"`
	SessionID    string              `json:"sessionId"`
	Repositories []RepositoryHandoff `json:"repositories"`
	Summary      string              `json:"summary"`
	Verification []string            `json:"verification"`
}

func (result WriteChildResult) Validate() error {
	if result.Kind != ChildKindWrite {
		return NewError(ErrorInvalidRequest, "write result kind must be write")
	}
	return nil
}
