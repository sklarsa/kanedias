//go:build incus

package supervisor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

func TestLiveRPCDeterministicChildLifecycle(t *testing.T) {
	runLifecycleScenario(t, "deterministic-children", func(h *liveAcceptance) {
		h.exerciseDeterministicChildren()
	})
}

func TestLiveRPCChildStopLifecycle(t *testing.T) {
	runLifecycleScenario(t, "child-stop", func(h *liveAcceptance) {
		h.exerciseLifecycleChildStop()
	})
}

func TestLiveRPCRootEndLifecycle(t *testing.T) {
	runLifecycleScenario(t, "root-end", func(h *liveAcceptance) {
		h.exerciseLifecycleRootEnd()
	})
}

func (h *liveAcceptance) exerciseDeterministicChildren() {
	root := h.startLifecycleRoot("deterministic-children")
	singleMarker := "KANEDIAS_LIFECYCLE_DIRECT_SINGLE_" + h.prefix
	singleCall := h.startLifecycleChildCall(root.client, root.tree.SessionID, "direct-single",
		"Read README.md and go.mod in the repository, then respond with the exact marker "+singleMarker+" and a concise summary of what you read.")

	singleTree := h.waitForLifecycleChildren(root, 1, false, "single bound child")
	singleChild := singleTree.Children[0]
	h.assertLifecycleChildren(root.tree, singleTree.Children)
	singleResult := singleCall.wait(h.t, 8*time.Minute)
	h.assertLifecycleReadResult(singleResult, singleChild.SessionID, singleMarker)
	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{singleChild}, "single child natural completion")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_DIRECT_SINGLE_ROOT_USABLE_"+h.prefix)

	parallelCalls := make([]*lifecycleChildCall, 3)
	parallelMarkers := make([]string, 3)
	for index := range parallelCalls {
		parallelMarkers[index] = fmt.Sprintf("KANEDIAS_LIFECYCLE_DIRECT_PARALLEL_%d_%s", index, h.prefix)
		parallelCalls[index] = h.startLifecycleChildCall(root.client, root.tree.SessionID,
			fmt.Sprintf("direct-parallel-%d", index),
			"Read README.md, go.mod, and at least five Go source files in the repository. Return a concise repository summary containing the exact marker "+parallelMarkers[index]+".")
	}

	parallelTree := h.waitForLifecycleChildren(root, 3, false, "three parallel bound children in one tree snapshot")
	h.assertLifecycleChildren(root.tree, parallelTree.Children)
	observedIDs := make(map[string]struct{}, len(parallelTree.Children))
	for _, child := range parallelTree.Children {
		observedIDs[child.SessionID] = struct{}{}
	}
	resultIDs := make(map[string]struct{}, len(parallelCalls))
	for index, call := range parallelCalls {
		result := call.wait(h.t, 8*time.Minute)
		readResult := h.assertLifecycleReadResult(result, "", parallelMarkers[index])
		if _, ok := observedIDs[readResult.SessionID]; !ok {
			h.t.Fatalf("parallel call %d returned unobserved child session %q", index, readResult.SessionID)
		}
		if _, duplicate := resultIDs[readResult.SessionID]; duplicate {
			h.t.Fatalf("parallel calls returned duplicate child session %q", readResult.SessionID)
		}
		resultIDs[readResult.SessionID] = struct{}{}
	}
	h.waitForLifecycleChildrenGone(root, parallelTree.Children, "parallel child natural completion")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_DIRECT_PARALLEL_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) exerciseLifecycleChildStop() {
	root := h.startLifecycleRoot("child-stop")
	call := h.startLifecycleChildCall(root.client, root.tree.SessionID, "active-child-stop",
		"Inspect the repository sources. Before writing any response, run the shell command `sleep 600`; after it completes, return KANEDIAS_LIFECYCLE_CHILD_STOP_UNEXPECTED_SUCCESS_"+h.prefix+".")

	activeTree := h.waitForLifecycleChildren(root, 1, true, "running child before direct stop")
	child := activeTree.Children[0]
	h.assertLifecycleChildren(root.tree, activeTree.Children)
	h.poll(2*time.Minute, "child agent_start before direct stop", func() bool {
		return root.journal.countPi(child.SessionID, "agent_start", "") >= 1
	})

	status, _, err := unixRequest(root.client, http.MethodDelete,
		"/v1/sessions/"+url.PathEscape(child.SessionID), nil)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("active child DELETE = %d, %v", status, err)
	}
	result := call.wait(h.t, 2*time.Minute)
	observed := h.assertLifecycleTypedError(result)
	h.writeJSON("child-stop-observed-error.json", map[string]any{
		"status": result.Status,
		"code":   observed.Code,
	})

	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{child}, "directly stopped child cleanup")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_CHILD_STOP_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) exerciseLifecycleRootEnd() {
	root := h.startLifecycleRoot("root-end")
	calls := make([]*lifecycleChildCall, 3)
	for index := range calls {
		marker := fmt.Sprintf("KANEDIAS_LIFECYCLE_ROOT_END_UNEXPECTED_SUCCESS_%d_%s", index, h.prefix)
		calls[index] = h.startLifecycleChildCall(root.client, root.tree.SessionID,
			fmt.Sprintf("root-end-active-%d", index),
			"Inspect the repository sources. Before writing any response, run the shell command `sleep 600`; after it completes, return "+marker+".")
	}

	activeTree := h.waitForLifecycleChildren(root, 3, true, "three running children before root end")
	h.assertLifecycleChildren(root.tree, activeTree.Children)
	for _, child := range activeTree.Children {
		h.poll(2*time.Minute, "agent_start for root-end child "+child.SessionID, func() bool {
			return root.journal.countPi(child.SessionID, "agent_start", "") >= 1
		})
	}
	h.trackTree(activeTree)
	allSessionIDs := treeSessionIDs(activeTree)
	descendantProcesses := descendantPIDs(root.process.cmd.Process.Pid)
	directChildren := directSessionChildPIDs(root.process.cmd.Process.Pid)
	if len(directChildren) != 3 || len(descendantProcesses) < 3 {
		h.t.Fatalf("root-end process tree has direct children=%v descendants=%v, want three active child processes", directChildren, descendantProcesses)
	}
	h.writeJSON("root-end-active-tree.json", map[string]any{
		"tree":              activeTree,
		"rootPid":           root.process.cmd.Process.Pid,
		"directChildPids":   directChildren,
		"descendantPids":    descendantProcesses,
		"trackedSessionIds": allSessionIDs,
	})

	status, _, err := unixRequest(root.client, http.MethodDelete,
		"/v1/sessions/"+url.PathEscape(root.tree.SessionID), nil)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("root-end root DELETE = %d, %v", status, err)
	}
	if err := h.waitProcess(root.process, 2*time.Minute); err != nil {
		h.t.Fatalf("root-end root exited after DELETE: %v", err)
	}
	if root.stalled != nil {
		_ = root.stalled.Close()
	}
	for _, call := range calls {
		_ = call.wait(h.t, 2*time.Minute)
	}

	h.poll(2*time.Minute, "root-end process, socket, and resource cleanup", func() bool {
		if pathExists(root.socket) || !h.sessionsAbsent(allSessionIDs) {
			return false
		}
		for _, child := range activeTree.Children {
			if pathExists(h.recursiveDescendantSocketPath(child.SessionID)) {
				return false
			}
		}
		for _, pid := range descendantProcesses {
			if pathExists(filepath.Join("/proc", fmt.Sprint(pid))) {
				return false
			}
		}
		return true
	})
	h.assertBaseline("root-end-four-session-baseline")
	if status, body, err := unixRequest(root.client, http.MethodGet, "/v1/tree", nil); err == nil {
		h.t.Fatalf("request through ended root socket unexpectedly succeeded: status=%d body=%q", status, body)
	}
}

func (h *liveAcceptance) waitForLifecycleChildren(root *lifecycleRoot, count int, requireRunning bool, description string) supervisor.NodeSnapshot {
	var observed supervisor.NodeSnapshot
	h.poll(8*time.Minute, description, func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) != count {
			return false
		}
		for _, child := range current.Children {
			if !isControllableChildSnapshot(child, contract.ChildKindRead, contract.ContextFresh) {
				return false
			}
			if requireRunning && child.Lifecycle != string(supervisor.LifecycleRunning) {
				return false
			}
		}
		observed = current
		return true
	})
	h.trackTree(observed)
	h.writeJSON(safeName(description)+"-tree.json", observed)
	return observed
}

func (h *liveAcceptance) assertLifecycleChildren(root supervisor.NodeSnapshot, children []supervisor.NodeSnapshot) {
	seenSessionIDs := make(map[string]struct{}, len(children))
	seenPiSessionIDs := make(map[string]struct{}, len(children))
	seenSessionFiles := make(map[string]struct{}, len(children))
	seenInstances := make(map[string]struct{}, len(children))
	seenVolumes := make(map[string]struct{}, len(children))
	for _, child := range children {
		if child.ParentSessionID != root.SessionID || child.RootSessionID != root.SessionID {
			h.t.Fatalf("child is not bound directly to lifecycle root: root=%q child=%#v", root.SessionID, child)
		}
		h.assertWorker(child, "reviewer")
		h.assertSessionMetadata(child)
		h.assertDistinct(root, child)
		instance := h.instanceForSession(child.SessionID)
		volume := h.volumeForSession(child.SessionID)
		h.recordDistinctLifecycleValue(seenSessionIDs, "session ID", child.SessionID)
		h.recordDistinctLifecycleValue(seenPiSessionIDs, "Pi session ID", child.PiSessionID)
		h.recordDistinctLifecycleValue(seenSessionFiles, "session file", child.SessionFile)
		h.recordDistinctLifecycleValue(seenInstances, "instance", instance)
		h.recordDistinctLifecycleValue(seenVolumes, "volume", volume)
	}
}

func (h *liveAcceptance) recordDistinctLifecycleValue(seen map[string]struct{}, kind, value string) {
	if _, duplicate := seen[value]; duplicate {
		h.t.Fatalf("lifecycle children have duplicate %s %q", kind, value)
	}
	seen[value] = struct{}{}
}

func (h *liveAcceptance) assertLifecycleReadResult(result lifecycleHTTPResult, wantSessionID, marker string) contract.ReadChildResult {
	if result.Err != nil || result.Status != http.StatusOK {
		h.t.Fatalf("terminal read result = status %d, error %v, body %q", result.Status, result.Err, result.Body)
	}
	var readResult contract.ReadChildResult
	if err := json.Unmarshal(result.Body, &readResult); err != nil {
		h.t.Fatalf("decode terminal read result: %v; body=%q", err, result.Body)
	}
	if readResult.Kind != contract.ChildKindRead || readResult.WorkerType != "reviewer" || readResult.SessionID == "" {
		h.t.Fatalf("terminal read result has invalid identity: %#v", readResult)
	}
	if wantSessionID != "" && readResult.SessionID != wantSessionID {
		h.t.Fatalf("terminal read session = %q, want %q", readResult.SessionID, wantSessionID)
	}
	if !strings.Contains(readResult.Output, marker) {
		h.t.Fatalf("terminal read output %q does not contain marker %q", readResult.Output, marker)
	}
	return readResult
}

func (h *liveAcceptance) assertLifecycleTypedError(result lifecycleHTTPResult) contract.Error {
	if result.Err != nil {
		h.t.Fatalf("stopped child call returned transport error instead of typed JSON: %v", result.Err)
	}
	if result.Status == http.StatusOK {
		h.t.Fatalf("stopped child call returned invalid success: %q", result.Body)
	}
	var typed contract.Error
	if err := json.Unmarshal(result.Body, &typed); err != nil {
		h.t.Fatalf("decode stopped child typed error: %v; body=%q", err, result.Body)
	}
	if !isCanonicalLifecycleErrorCode(typed.Code) || typed.Message == "" || typed.Code.HTTPStatus() != result.Status {
		h.t.Fatalf("stopped child error is not canonical: status=%d error=%#v", result.Status, typed)
	}
	return typed
}

func isCanonicalLifecycleErrorCode(code contract.ErrorCode) bool {
	switch code {
	case contract.ErrorInvalidRequest,
		contract.ErrorUnknownWorkerType,
		contract.ErrorForbiddenRPC,
		contract.ErrorProxyUnavailable,
		contract.ErrorWorkspaceRepositoryUnavailable,
		contract.ErrorProvisioningFailed,
		contract.ErrorChildFailed,
		contract.ErrorChildAborted,
		contract.ErrorHandoffRefMissing,
		contract.ErrorHandoffRefMismatch,
		contract.ErrorSessionStopping,
		contract.ErrorNotFound,
		contract.ErrorChildUnavailable,
		contract.ErrorConflict,
		contract.ErrorSaturated,
		contract.ErrorInternal:
		return true
	default:
		return false
	}
}

func (h *liveAcceptance) waitForLifecycleChildrenGone(root *lifecycleRoot, children []supervisor.NodeSnapshot, description string) {
	ids := make([]string, 0, len(children))
	for _, child := range children {
		ids = append(ids, child.SessionID)
	}
	h.poll(2*time.Minute, description, func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) != 0 || !h.sessionsAbsent(ids) {
			return false
		}
		for _, id := range ids {
			if pathExists(h.recursiveDescendantSocketPath(id)) {
				return false
			}
		}
		return true
	})
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
