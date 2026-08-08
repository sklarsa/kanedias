package manager

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// knownLifecycles is the set of lifecycle strings accepted from a root
// supervisor snapshot.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

var knownLifecycles = map[string]struct{}{
	string(supervisor.LifecycleProvisioning):    {},
	string(supervisor.LifecycleStarting):        {},
	string(supervisor.LifecycleReady):           {},
	string(supervisor.LifecycleRunning):         {},
	string(supervisor.LifecycleAwaitingHandoff): {},
	string(supervisor.LifecycleCompleted):       {},
	string(supervisor.LifecycleFailed):          {},
	string(supervisor.LifecycleStopping):        {},
	string(supervisor.LifecycleStopped):         {},
}

type treeWork struct {
	node     *supervisor.NodeSnapshot
	parentID string
	rootID   string
}

// validateRootTree validates the full tree rooted at root. It confirms:
//   - the top node has Kind==root and RootSessionID==SessionID;
//   - every descendant has a valid lifecycle;
//   - no session ID appears more than once;
//   - every child's RootSessionID matches the root;
//   - every node's ParentSessionID matches its expected parent.
//
// It normalises children by sorting by SessionID and returns the normalised
// tree together with a complete routes map (sessionID -> rootID).
//
// Admitted top roots must have nonempty PiSessionID; starting descendants
// are allowed without Pi binding.
func validateRootTree(root supervisor.NodeSnapshot) (supervisor.NodeSnapshot, map[string]string, error) {
	if root.Kind != contract.ChildKindRoot {
		return supervisor.NodeSnapshot{}, nil, fmt.Errorf("top-level node kind %q is not root", root.Kind)
	}
	if root.RootSessionID != root.SessionID {
		return supervisor.NodeSnapshot{}, nil, fmt.Errorf("root node RootSessionID %q != SessionID %q", root.RootSessionID, root.SessionID)
	}
	if root.SessionID == "" {
		return supervisor.NodeSnapshot{}, nil, fmt.Errorf("root node has empty SessionID")
	}

	routes := map[string]string{}
	work := []treeWork{{node: &root, parentID: "", rootID: root.SessionID}}
	for len(work) > 0 {
		item := work[len(work)-1]
		work = work[:len(work)-1]

		node := item.node

		if !validSessionID(node.SessionID) {
			return supervisor.NodeSnapshot{}, nil, fmt.Errorf("session ID %q is not a safe URL path segment", node.SessionID)
		}
		if _, dup := routes[node.SessionID]; dup {
			return supervisor.NodeSnapshot{}, nil, fmt.Errorf("duplicate session ID %q in tree", node.SessionID)
		}
		if _, ok := knownLifecycles[node.Lifecycle]; !ok {
			return supervisor.NodeSnapshot{}, nil, fmt.Errorf("unknown lifecycle %q for session %q", node.Lifecycle, node.SessionID)
		}
		if node.RootSessionID != item.rootID {
			return supervisor.NodeSnapshot{}, nil, fmt.Errorf("session %q has RootSessionID %q, expected %q", node.SessionID, node.RootSessionID, item.rootID)
		}
		if item.parentID != "" && node.ParentSessionID != item.parentID {
			return supervisor.NodeSnapshot{}, nil, fmt.Errorf("session %q has ParentSessionID %q, expected %q", node.SessionID, node.ParentSessionID, item.parentID)
		}
		// Non-root nodes must not carry Kind==root
		if item.parentID != "" && node.Kind == contract.ChildKindRoot {
			return supervisor.NodeSnapshot{}, nil, fmt.Errorf("descendant session %q has root kind", node.SessionID)
		}

		routes[node.SessionID] = item.rootID

		// Sort children stably before enqueueing so the normalised tree is
		// deterministic and comparable.
		sort.Slice(node.Children, func(i, j int) bool {
			return node.Children[i].SessionID < node.Children[j].SessionID
		})
		for i := range node.Children {
			work = append(work, treeWork{
				node:     &node.Children[i],
				parentID: node.SessionID,
				rootID:   item.rootID,
			})
		}
	}

	return root, routes, nil
}

func validSessionID(id string) bool {
	return id != "." && id != ".." && sessionIDPattern.MatchString(id)
}

// admissible returns true when the root snapshot has a lifecycle that the
// manager treats as fully admitted (ready or running) and the Pi binding is
// populated.
func admissible(snapshot supervisor.NodeSnapshot) bool {
	lc := supervisor.LifecycleState(snapshot.Lifecycle)
	return (lc == supervisor.LifecycleReady || lc == supervisor.LifecycleRunning) &&
		snapshot.PiSessionID != "" && snapshot.SessionFile != ""
}

// retainable returns true when a stopping/failed snapshot should be kept
// visible but marked non-actionable.
func retainable(snapshot supervisor.NodeSnapshot) bool {
	lc := supervisor.LifecycleState(snapshot.Lifecycle)
	return lc == supervisor.LifecycleStopping || lc == supervisor.LifecycleFailed ||
		lc == supervisor.LifecycleStopped || lc == supervisor.LifecycleCompleted
}
