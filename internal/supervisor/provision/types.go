package provision

import (
	"context"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

type Resources struct {
	SessionID string
	Pool      string
	Instance  string
	Volume    string
	RPCAddr   string
}

type SocketIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

// RecoveryTicket is held only by the admitted direct parent and is usable only
// after that exact child process has exited.
type RecoveryTicket struct {
	SessionID      string               `json:"sessionId"`
	ParentID       string               `json:"parentId"`
	RootID         string               `json:"rootId"`
	Pool           string               `json:"pool"`
	Instance       string               `json:"instance"`
	Volume         string               `json:"volume"`
	SocketPath     string               `json:"socketPath"`
	Socket         SocketIdentity       `json:"socket"`
	Kind           contract.ChildKind   `json:"kind"`
	Context        contract.ContextMode `json:"context"`
	WorkerType     string               `json:"workerType"`
	RunAttribution string               `json:"runAttribution,omitempty"`
}

type DirectChildRecoverer interface {
	RecoverDirectChild(context.Context, RecoveryTicket) error
}

type RootRequest struct {
	SessionID      string
	SocketPath     string
	Model          config.ModelProfile
	RunAttribution string
}

type ChildRequest struct {
	SessionID      string
	ParentID       string
	RootID         string
	SourceInstance string
	SourceVolume   string
	HostSocketPath string
	Worker         config.WorkerProfile
	Contract       contract.CreateChildRequest
	RunAttribution string
}

type RootProvisioner interface {
	ProvisionRoot(context.Context, RootRequest) (*Resources, error)
	Destroy(context.Context, *Resources) error
}

type ChildProvisioner interface {
	ProvisionChild(context.Context, ChildRequest) (*Resources, error)
	Destroy(context.Context, *Resources) error
}
