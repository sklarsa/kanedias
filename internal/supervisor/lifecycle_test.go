package supervisor

import (
	"errors"
	"testing"
)

func TestLifecycleAllowsOnlyApprovedTransitions(t *testing.T) {
	legal := map[LifecycleState][]LifecycleState{
		LifecycleProvisioning:    {LifecycleStarting, LifecycleFailed, LifecycleStopping},
		LifecycleStarting:        {LifecycleReady, LifecycleFailed, LifecycleStopping},
		LifecycleReady:           {LifecycleRunning, LifecycleCompleted, LifecycleFailed, LifecycleStopping},
		LifecycleRunning:         {LifecycleReady, LifecycleAwaitingHandoff, LifecycleCompleted, LifecycleFailed, LifecycleStopping},
		LifecycleAwaitingHandoff: {LifecycleRunning, LifecycleCompleted, LifecycleFailed, LifecycleStopping},
		LifecycleCompleted:       {LifecycleStopping},
		LifecycleFailed:          {LifecycleStopping},
		LifecycleStopping:        {LifecycleStopped},
	}

	for from, destinations := range legal {
		for _, to := range destinations {
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				lifecycle := newLifecycle(from)
				if err := lifecycle.Transition(to); err != nil {
					t.Fatalf("Transition(%q -> %q) error = %v", from, to, err)
				}
				if got := lifecycle.State(); got != to {
					t.Fatalf("State() = %q, want %q", got, to)
				}
			})
		}
	}
}

func TestLifecycleRejectsEveryTransitionOutsideApprovedGraph(t *testing.T) {
	legal := map[LifecycleState]map[LifecycleState]bool{
		LifecycleProvisioning:    {LifecycleStarting: true, LifecycleFailed: true, LifecycleStopping: true},
		LifecycleStarting:        {LifecycleReady: true, LifecycleFailed: true, LifecycleStopping: true},
		LifecycleReady:           {LifecycleRunning: true, LifecycleCompleted: true, LifecycleFailed: true, LifecycleStopping: true},
		LifecycleRunning:         {LifecycleReady: true, LifecycleAwaitingHandoff: true, LifecycleCompleted: true, LifecycleFailed: true, LifecycleStopping: true},
		LifecycleAwaitingHandoff: {LifecycleRunning: true, LifecycleCompleted: true, LifecycleFailed: true, LifecycleStopping: true},
		LifecycleCompleted:       {LifecycleStopping: true},
		LifecycleFailed:          {LifecycleStopping: true},
		LifecycleStopping:        {LifecycleStopped: true},
	}
	states := []LifecycleState{LifecycleProvisioning, LifecycleStarting, LifecycleReady, LifecycleRunning, LifecycleAwaitingHandoff, LifecycleCompleted, LifecycleFailed, LifecycleStopping, LifecycleStopped}
	for _, from := range states {
		for _, to := range states {
			if legal[from][to] {
				continue
			}
			t.Run(string(from)+"_to_"+string(to), func(t *testing.T) {
				lifecycle := newLifecycle(from)
				err := lifecycle.Transition(to)
				if !errors.Is(err, ErrInvariant) {
					t.Fatalf("Transition(%q -> %q) error = %v, want ErrInvariant", from, to, err)
				}
				if got := lifecycle.State(); got != from {
					t.Fatalf("State() = %q after rejected transition, want %q", got, from)
				}
			})
		}
	}
}

func TestLifecycleRejectsUnknownStateTransitions(t *testing.T) {
	tests := []struct {
		name string
		from LifecycleState
		to   LifecycleState
	}{
		{name: "unknown destination", from: LifecycleReady, to: "unknown"},
		{name: "unknown source", from: "unknown", to: LifecycleReady},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lifecycle := newLifecycle(tt.from)
			err := lifecycle.Transition(tt.to)
			if !errors.Is(err, ErrInvariant) {
				t.Fatalf("Transition(%q -> %q) error = %v, want ErrInvariant", tt.from, tt.to, err)
			}
			if got := lifecycle.State(); got != tt.from {
				t.Fatalf("State() = %q after rejected transition, want %q", got, tt.from)
			}
		})
	}
}
