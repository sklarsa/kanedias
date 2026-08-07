package supervisor

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

var ErrInvariant = errors.New("supervisor invariant violation")

type Identity struct {
	sessionID string
	parentID  string
	rootID    string
	kind      contract.ChildKind
	context   contract.ContextMode
	worker    string
}

type IdentitySpec struct {
	SessionID string
	ParentID  string
	RootID    string
	Kind      contract.ChildKind
	Context   contract.ContextMode
	Worker    string
}

type IdentitySnapshot struct {
	SessionID string
	ParentID  string
	RootID    string
	Kind      contract.ChildKind
	Context   contract.ContextMode
	Worker    string
}

type PiBinding struct {
	SessionID   string `json:"sessionId"`
	SessionFile string `json:"sessionFile"`
}

type WorkerCatalog interface {
	Resolve(name string) (config.WorkerProfile, error)
	Summaries() []contract.WorkerSummary
}

func NewIdentity(spec IdentitySpec) (Identity, error) {
	if strings.TrimSpace(spec.SessionID) == "" {
		return Identity{}, invariantf("session ID is required")
	}
	if strings.TrimSpace(spec.RootID) == "" {
		return Identity{}, invariantf("root session ID is required")
	}

	switch spec.Kind {
	case contract.ChildKindRoot:
		if spec.RootID != spec.SessionID {
			return Identity{}, invariantf("root session must own itself")
		}
		if spec.ParentID != "" {
			return Identity{}, invariantf("root session cannot have a parent")
		}
		if spec.Context != contract.ContextRoot {
			return Identity{}, invariantf("root session must use root context")
		}
		if spec.Worker != "" {
			return Identity{}, invariantf("root session cannot have a worker type")
		}
	case contract.ChildKindRead, contract.ChildKindWrite:
		if strings.TrimSpace(spec.ParentID) == "" {
			return Identity{}, invariantf("child session parent ID is required")
		}
		if spec.Context != contract.ContextFresh && spec.Context != contract.ContextFork {
			return Identity{}, invariantf("child session context must be fresh or fork")
		}
		if strings.TrimSpace(spec.Worker) == "" {
			return Identity{}, invariantf("child session worker type is required")
		}
	default:
		return Identity{}, invariantf("unknown child kind %q", spec.Kind)
	}

	return Identity{
		sessionID: spec.SessionID,
		parentID:  spec.ParentID,
		rootID:    spec.RootID,
		kind:      spec.Kind,
		context:   spec.Context,
		worker:    spec.Worker,
	}, nil
}

func (identity Identity) Snapshot() IdentitySnapshot {
	return IdentitySnapshot{
		SessionID: identity.sessionID,
		ParentID:  identity.parentID,
		RootID:    identity.rootID,
		Kind:      identity.kind,
		Context:   identity.context,
		Worker:    identity.worker,
	}
}

func invariantf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvariant, fmt.Sprintf(format, args...))
}
