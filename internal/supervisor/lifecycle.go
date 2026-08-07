package supervisor

import "sync"

type LifecycleState string

const (
	LifecycleProvisioning    LifecycleState = "provisioning"
	LifecycleStarting        LifecycleState = "starting"
	LifecycleReady           LifecycleState = "ready"
	LifecycleRunning         LifecycleState = "running"
	LifecycleAwaitingHandoff LifecycleState = "awaiting_handoff"
	LifecycleCompleted       LifecycleState = "completed"
	LifecycleFailed          LifecycleState = "failed"
	LifecycleStopping        LifecycleState = "stopping"
	LifecycleStopped         LifecycleState = "stopped"
)

type lifecycle struct {
	mu    sync.RWMutex
	state LifecycleState
}

var legalLifecycleTransitions = map[LifecycleState]map[LifecycleState]struct{}{
	LifecycleProvisioning: {
		LifecycleStarting: {}, LifecycleFailed: {}, LifecycleStopping: {},
	},
	LifecycleStarting: {
		LifecycleReady: {}, LifecycleFailed: {}, LifecycleStopping: {},
	},
	LifecycleReady: {
		LifecycleRunning: {}, LifecycleCompleted: {}, LifecycleFailed: {}, LifecycleStopping: {},
	},
	LifecycleRunning: {
		LifecycleReady: {}, LifecycleAwaitingHandoff: {}, LifecycleCompleted: {}, LifecycleFailed: {}, LifecycleStopping: {},
	},
	LifecycleAwaitingHandoff: {
		LifecycleRunning: {}, LifecycleCompleted: {}, LifecycleFailed: {}, LifecycleStopping: {},
	},
	LifecycleCompleted: {LifecycleStopping: {}},
	LifecycleFailed:    {LifecycleStopping: {}},
	LifecycleStopping:  {LifecycleStopped: {}},
}

func newLifecycle(initial LifecycleState) *lifecycle {
	return &lifecycle{state: initial}
}

func (lifecycle *lifecycle) State() LifecycleState {
	lifecycle.mu.RLock()
	defer lifecycle.mu.RUnlock()
	return lifecycle.state
}

func (lifecycle *lifecycle) Transition(next LifecycleState) error {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()

	if _, ok := legalLifecycleTransitions[lifecycle.state][next]; !ok {
		return invariantf("illegal lifecycle transition %q -> %q", lifecycle.state, next)
	}
	lifecycle.state = next
	return nil
}
