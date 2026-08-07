package supervisor

import (
	"context"
	"time"
)

type StopReason string

const (
	StopReasonRequested  StopReason = "requested"
	StopReasonParent     StopReason = "parent_stopped"
	StopReasonCancelled  StopReason = "cancelled"
	StopReasonRPCFailure StopReason = "rpc_failure"

	stopCleanupTimeout = 30 * time.Second
)

func (node *Node) Stop(ctx context.Context, _ StopReason) error {
	node.requestStop(ctx)
	select {
	case <-node.done:
		return node.finishedError()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (node *Node) finalizeStop(detached context.Context) {
	<-node.startupDone
	cleanupCtx, cancel := context.WithTimeout(detached, stopCleanupTimeout)
	defer cancel()
	node.finish(cleanupCtx, nil, LifecycleStopped, true)
}
