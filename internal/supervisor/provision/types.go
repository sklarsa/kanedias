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

type RootRequest struct {
	SessionID  string
	SocketPath string
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
}

type RootProvisioner interface {
	ProvisionRoot(context.Context, RootRequest) (*Resources, error)
	Destroy(context.Context, *Resources) error
}

type ChildProvisioner interface {
	ProvisionChild(context.Context, ChildRequest) (*Resources, error)
	Destroy(context.Context, *Resources) error
}
