package supervisor

import "context"

type StopReason string

const (
	StopReasonRequested  StopReason = "requested"
	StopReasonParent     StopReason = "parent_stopped"
	StopReasonCancelled  StopReason = "cancelled"
	StopReasonRPCFailure StopReason = "rpc_failure"
)

func (node *Node) Stop(ctx context.Context, _ StopReason) error {
	node.finish(ctx, nil, LifecycleStopped, true)
	return node.finishedError()
}
