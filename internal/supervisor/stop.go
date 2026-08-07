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
	if startupDone := node.requestStop(); startupDone != nil {
		select {
		case <-startupDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	node.finish(ctx, nil, LifecycleStopped, true)
	return node.finishedError()
}
