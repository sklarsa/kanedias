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

func TestLiveRPCModelChildLifecycle(t *testing.T) {
	runLifecycleScenario(t, "model-children", func(h *liveAcceptance) {
		h.exerciseModelDirectedChildren()
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

func TestLiveRPCInterruptLifecycle(t *testing.T) {
	runLifecycleScenario(t, "interrupt", func(h *liveAcceptance) {
		h.exerciseLifecycleInterrupt()
	})
}

func TestLiveRPCSteerLifecycle(t *testing.T) {
	runLifecycleScenario(t, "steer", func(h *liveAcceptance) {
		h.exerciseLifecycleSteer()
	})
}

func TestLiveRPCRapidControlLifecycle(t *testing.T) {
	runLifecycleScenario(t, "rapid-control", func(h *liveAcceptance) {
		h.exerciseLifecycleRapidControl()
	})
}

func TestValidateLifecycleStoppedResultRequiresExactChildAborted(t *testing.T) {
	valid := lifecycleHTTPResult{
		Status: contract.ErrorChildAborted.HTTPStatus(),
		Body:   []byte(`{"code":"child_aborted","message":"child was stopped"}`),
	}
	if _, err := validateLifecycleStoppedResult(valid); err != nil {
		t.Fatalf("valid stopped result: %v", err)
	}

	unrelated := []contract.ErrorCode{
		contract.ErrorInvalidRequest,
		contract.ErrorUnknownWorkerType,
		contract.ErrorForbiddenRPC,
		contract.ErrorProxyUnavailable,
		contract.ErrorWorkspaceRepositoryUnavailable,
		contract.ErrorProvisioningFailed,
		contract.ErrorChildFailed,
		contract.ErrorHandoffRefMissing,
		contract.ErrorHandoffRefMismatch,
		contract.ErrorSessionStopping,
		contract.ErrorNotFound,
		contract.ErrorChildUnavailable,
		contract.ErrorConflict,
		contract.ErrorSaturated,
		contract.ErrorInternal,
	}
	for _, code := range unrelated {
		t.Run(string(code), func(t *testing.T) {
			result := lifecycleHTTPResult{
				Status: code.HTTPStatus(),
				Body:   []byte(fmt.Sprintf(`{"code":%q,"message":"unrelated"}`, code)),
			}
			if _, err := validateLifecycleStoppedResult(result); err == nil {
				t.Fatalf("unrelated stopped result code %q was accepted", code)
			}
		})
	}

	for _, test := range []struct {
		name   string
		result lifecycleHTTPResult
	}{
		{name: "success", result: lifecycleHTTPResult{Status: http.StatusOK, Body: []byte(`{"kind":"read"}`)}},
		{name: "wrong status", result: lifecycleHTTPResult{Status: http.StatusBadGateway, Body: valid.Body}},
		{name: "empty message", result: lifecycleHTTPResult{Status: valid.Status, Body: []byte(`{"code":"child_aborted","message":""}`)}},
		{name: "malformed JSON", result: lifecycleHTTPResult{Status: valid.Status, Body: []byte(`{`)}},
		{name: "transport error", result: lifecycleHTTPResult{Err: errors.New("connection closed")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := validateLifecycleStoppedResult(test.result); err == nil {
				t.Fatal("invalid stopped result was accepted")
			}
		})
	}
}

func TestValidateLifecycleLeafTopologyRequiresExactDirectLeaves(t *testing.T) {
	valid := lifecycleLeafTree(3)
	if err := validateLifecycleLeafTopology(valid, 3); err != nil {
		t.Fatalf("valid leaf topology: %v", err)
	}

	for _, test := range []struct {
		name string
		edit func(*supervisor.NodeSnapshot)
	}{
		{name: "too few children", edit: func(tree *supervisor.NodeSnapshot) { tree.Children = tree.Children[:2] }},
		{name: "too many children", edit: func(tree *supervisor.NodeSnapshot) {
			tree.Children = append(tree.Children, lifecycleLeafChild("child-4"))
		}},
		{name: "grandchild", edit: func(tree *supervisor.NodeSnapshot) {
			tree.Children[0].Children = []supervisor.NodeSnapshot{lifecycleLeafChild("grandchild")}
		}},
		{name: "wrong parent", edit: func(tree *supervisor.NodeSnapshot) { tree.Children[0].ParentSessionID = "other" }},
		{name: "wrong root", edit: func(tree *supervisor.NodeSnapshot) { tree.Children[0].RootSessionID = "other" }},
		{name: "duplicate session", edit: func(tree *supervisor.NodeSnapshot) { tree.Children[1].SessionID = tree.Children[0].SessionID }},
	} {
		t.Run(test.name, func(t *testing.T) {
			tree := lifecycleLeafTree(3)
			test.edit(&tree)
			if err := validateLifecycleLeafTopology(tree, 3); err == nil {
				t.Fatal("invalid leaf topology was accepted")
			}
		})
	}
}

func lifecycleLeafTree(children int) supervisor.NodeSnapshot {
	tree := supervisor.NodeSnapshot{SessionID: "root", RootSessionID: "root"}
	for index := 0; index < children; index++ {
		tree.Children = append(tree.Children, lifecycleLeafChild(fmt.Sprintf("child-%d", index+1)))
	}
	return tree
}

func lifecycleLeafChild(id string) supervisor.NodeSnapshot {
	return supervisor.NodeSnapshot{SessionID: id, ParentSessionID: "root", RootSessionID: "root"}
}

func (h *liveAcceptance) exerciseDeterministicChildren() {
	root := h.startLifecycleRoot("deterministic-children")
	singleMarker := "KANEDIAS_LIFECYCLE_DIRECT_SINGLE_" + h.prefix
	singleCall := h.startLifecycleChildCall(root.client, root.tree.SessionID, "direct-single",
		"Read README.md and go.mod in the repository, then respond with the exact marker "+singleMarker+" and a concise summary of what you read.")

	singleTree := h.waitForLifecycleChildren(root, 1, false, "single bound child")
	singleChild := singleTree.Children[0]
	singlePIDs := h.captureLifecycleChildPIDs(root, 1, "direct-single")
	h.assertLifecycleChildren(root.tree, singleTree.Children)
	singleResult := singleCall.wait(h.t, 8*time.Minute)
	h.assertLifecycleReadResult(singleResult, singleChild.SessionID, singleMarker)
	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{singleChild}, singlePIDs, "single child natural completion")
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
	parallelPIDs := h.captureLifecycleChildPIDs(root, 3, "direct-parallel")
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
	h.waitForLifecycleChildrenGone(root, parallelTree.Children, parallelPIDs, "parallel child natural completion")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_DIRECT_PARALLEL_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) exerciseModelDirectedChildren() {
	root := h.startLifecycleRoot("model-children")
	defer h.writeLifecycleModelEvidence(root, "model-children-final")

	singleMarker := "KANEDIAS_LIFECYCLE_MODEL_SINGLE_" + h.prefix
	singleBoundary := len(root.journal.snapshot())
	singleSettled := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	singlePrompt := "In your next assistant turn, call delegate_session exactly once with workerType reviewer, kind read, context fresh. " +
		"The child task is: inspect internal/supervisor/lifecycle.go and return exactly " + singleMarker + " after the inspection. " +
		"After the tool returns, reply with exactly " + singleMarker + "."
	h.sendLifecycleModelPrompt(root, singlePrompt)

	singleTree := h.waitForLifecycleModelChildren(root, singleBoundary, singleSettled, 1, "single model-directed bound child")
	singleChild := singleTree.Children[0]
	singlePIDs := h.captureLifecycleChildPIDs(root, 1, "model-single")
	h.assertLifecycleChildren(root.tree, singleTree.Children)
	h.waitForLifecycleRootSettlement(root, singleSettled, "single model-directed root settlement")
	singleEvents := root.journal.snapshot()[singleBoundary:]
	if err := validateLifecycleModelToolEvents(singleEvents, root.tree.SessionID, []string{singleMarker}, []string{singleChild.SessionID}, false); err != nil {
		h.t.Fatalf("single model-directed tool acceptance: %v", err)
	}
	if text := h.lastAssistantText(root.client, root.tree.SessionID); !strings.Contains(text, singleMarker) {
		h.t.Fatalf("single model-directed root final text %q lacks marker %q", text, singleMarker)
	}
	h.waitForLifecycleNaturalChildEvents(root, []supervisor.NodeSnapshot{singleChild}, "single model-directed child natural completion")
	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{singleChild}, singlePIDs, "single model-directed child disappearance")
	h.writeLifecycleModelEvidence(root, "model-single-complete")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_MODEL_SINGLE_ROOT_USABLE_"+h.prefix)

	parallelMarkers := []string{
		"KANEDIAS_LIFECYCLE_MODEL_PARALLEL_1_" + h.prefix,
		"KANEDIAS_LIFECYCLE_MODEL_PARALLEL_2_" + h.prefix,
		"KANEDIAS_LIFECYCLE_MODEL_PARALLEL_3_" + h.prefix,
	}
	parallelBoundary := len(root.journal.snapshot())
	parallelSettled := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	parallelPrompt := "In your next assistant turn, call delegate_session exactly three times in one parallel tool-call batch. " +
		"Use workerType reviewer, kind read, context fresh for every call. " +
		"Call 1 child task is: inspect README.md and return exactly " + parallelMarkers[0] + " after the inspection. " +
		"Call 2 child task is: inspect go.mod and return exactly " + parallelMarkers[1] + " after the inspection. " +
		"Call 3 child task is: inspect internal/supervisor/node.go and return exactly " + parallelMarkers[2] + " after the inspection. " +
		"After all three tools return, reply with exactly " + strings.Join(parallelMarkers, " ") + "."
	h.sendLifecycleModelPrompt(root, parallelPrompt)

	parallelTree := h.waitForLifecycleModelChildren(root, parallelBoundary, parallelSettled, 3, "three simultaneous model-directed children")
	parallelPIDs := h.captureLifecycleChildPIDs(root, 3, "model-parallel")
	h.assertLifecycleChildren(root.tree, parallelTree.Children)
	h.waitForLifecycleRootSettlement(root, parallelSettled, "parallel model-directed root settlement")
	parallelEvents := root.journal.snapshot()[parallelBoundary:]
	parallelChildIDs := make([]string, 0, len(parallelTree.Children))
	for _, child := range parallelTree.Children {
		parallelChildIDs = append(parallelChildIDs, child.SessionID)
	}
	if err := validateLifecycleModelToolEvents(parallelEvents, root.tree.SessionID, parallelMarkers, parallelChildIDs, true); err != nil {
		h.t.Fatalf("parallel model-directed tool acceptance: %v", err)
	}
	parallelText := h.lastAssistantText(root.client, root.tree.SessionID)
	for _, marker := range parallelMarkers {
		if !strings.Contains(parallelText, marker) {
			h.t.Fatalf("parallel model-directed aggregate text %q lacks marker %q", parallelText, marker)
		}
	}
	h.waitForLifecycleNaturalChildEvents(root, parallelTree.Children, "parallel model-directed children natural completion")
	h.waitForLifecycleChildrenGone(root, parallelTree.Children, parallelPIDs, "parallel model-directed child resource cleanup")
	h.writeLifecycleModelEvidence(root, "model-parallel-complete")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_MODEL_PARALLEL_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) exerciseLifecycleChildStop() {
	root := h.startLifecycleRoot("child-stop")
	call := h.startLifecycleChildCall(root.client, root.tree.SessionID, "active-child-stop",
		lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_CHILD_STOP_UNEXPECTED_SUCCESS_"+h.prefix))

	activeTree := h.waitForLifecycleChildren(root, 1, true, "running child before direct stop")
	child := activeTree.Children[0]
	h.assertLifecycleChildren(root.tree, activeTree.Children)
	childPIDs := h.captureLifecycleChildPIDs(root, 1, "active-child-stop")
	h.poll(2*time.Minute, "child agent_start before direct stop", func() bool {
		return root.journal.countPi(child.SessionID, "agent_start", "") >= 1
	})
	h.assertLifecycleChildStreaming(root, child.SessionID)

	status, _, err := unixRequest(root.client, http.MethodDelete,
		"/v1/sessions/"+url.PathEscape(child.SessionID), nil)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("active child DELETE = %d, %v", status, err)
	}
	result := call.wait(h.t, 2*time.Minute)
	observed := h.assertLifecycleStoppedResult(result)
	h.writeJSON("child-stop-observed-error.json", map[string]any{
		"status": result.Status,
		"code":   observed.Code,
	})

	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{child}, childPIDs, "directly stopped child cleanup")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_CHILD_STOP_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) exerciseLifecycleRootEnd() {
	root := h.startLifecycleRoot("root-end")
	calls := make([]*lifecycleChildCall, 3)
	for index := range calls {
		marker := fmt.Sprintf("KANEDIAS_LIFECYCLE_ROOT_END_UNEXPECTED_SUCCESS_%d_%s", index, h.prefix)
		calls[index] = h.startLifecycleChildCall(root.client, root.tree.SessionID,
			fmt.Sprintf("root-end-active-%d", index), lifecycleActiveReadTask(marker))
	}

	activeTree := h.waitForLifecycleChildren(root, 3, true, "three running children before root end")
	if err := validateLifecycleLeafTopology(activeTree, 3); err != nil {
		h.t.Fatalf("root-end topology: %v", err)
	}
	h.assertLifecycleChildren(root.tree, activeTree.Children)
	for _, child := range activeTree.Children {
		h.poll(2*time.Minute, "agent_start for root-end child "+child.SessionID, func() bool {
			return root.journal.countPi(child.SessionID, "agent_start", "") >= 1
		})
		h.assertLifecycleChildStreaming(root, child.SessionID)
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
		h.assertLifecycleStoppedResult(call.wait(h.t, 2*time.Minute))
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

func (h *liveAcceptance) exerciseLifecycleInterrupt() {
	root := h.startLifecycleRoot("interrupt")
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "prompt", "message": lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_INTERRUPT_ROOT_UNEXPECTED_" + h.prefix),
	})
	h.waitLifecycleStreaming(root, root.tree.SessionID, "root running and streaming before interrupt")
	rootSettledBefore := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{"type": "abort"})
	h.waitLifecycleSettlement(root, root.tree.SessionID, rootSettledBefore, false, "root interrupt settlement and open transport")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_INTERRUPT_ROOT_USABLE_"+h.prefix)
	if got := root.journal.countPi(root.tree.SessionID, "agent_settled", ""); got != rootSettledBefore+2 {
		h.t.Fatalf("root interrupt plus follow-up settlements = %d, want exactly 2", got-rootSettledBefore)
	}

	childCall := h.startLifecycleChildCall(root.client, root.tree.SessionID, "interrupt-child",
		lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_INTERRUPT_CHILD_UNEXPECTED_"+h.prefix))
	childTree := h.waitForLifecycleChildren(root, 1, true, "running child before interrupt")
	child := childTree.Children[0]
	h.assertLifecycleChildren(root.tree, childTree.Children)
	childPIDs := h.captureLifecycleChildPIDs(root, 1, "interrupt-child")
	h.waitLifecycleStreaming(root, child.SessionID, "child running and streaming before interrupt")
	childSettledBefore := root.journal.countPi(child.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, child.SessionID, map[string]any{"type": "abort"})
	h.waitLifecycleSettlement(root, child.SessionID, childSettledBefore, false, "child interrupt settlement and open transport")
	h.assertLifecycleStoppedResult(childCall.wait(h.t, 2*time.Minute))
	if got := root.journal.countPi(child.SessionID, "agent_settled", ""); got != childSettledBefore+1 {
		h.t.Fatalf("interrupted child settlements = %d, want exactly 1", got-childSettledBefore)
	}
	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{child}, childPIDs, "interrupted child cleanup")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_INTERRUPT_CHILD_ROOT_USABLE_"+h.prefix)
	if got := root.journal.countPi(root.tree.SessionID, "agent_settled", ""); got != rootSettledBefore+3 {
		h.t.Fatalf("interrupt scenario root settlements = %d, want exactly 3", got-rootSettledBefore)
	}
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) exerciseLifecycleSteer() {
	root := h.startLifecycleRoot("steer")
	rootSteerMarker := "KANEDIAS_LIFECYCLE_STEER_ROOT_" + h.prefix
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "prompt", "message": lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_STEER_ROOT_ORIGINAL_" + h.prefix),
	})
	h.waitLifecycleStreaming(root, root.tree.SessionID, "root running and streaming before steer")
	rootSettledBefore := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "steer", "message": "Stop the prior response and include exactly " + rootSteerMarker + ".",
	})
	h.waitLifecycleSettlement(root, root.tree.SessionID, rootSettledBefore, false, "steered root settlement and open transport")
	if text := h.lastAssistantText(root.client, root.tree.SessionID); !strings.Contains(text, rootSteerMarker) {
		h.t.Fatalf("steered root final text %q lacks marker %q", text, rootSteerMarker)
	}
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_STEER_ROOT_USABLE_"+h.prefix)
	if got := root.journal.countPi(root.tree.SessionID, "agent_settled", ""); got != rootSettledBefore+2 {
		h.t.Fatalf("root steer plus follow-up settlements = %d, want exactly 2", got-rootSettledBefore)
	}

	childSteerMarker := "KANEDIAS_LIFECYCLE_STEER_CHILD_" + h.prefix
	childCall := h.startLifecycleChildCall(root.client, root.tree.SessionID, "steer-child",
		lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_STEER_CHILD_ORIGINAL_"+h.prefix))
	childTree := h.waitForLifecycleChildren(root, 1, true, "running child before steer")
	child := childTree.Children[0]
	h.assertLifecycleChildren(root.tree, childTree.Children)
	childPIDs := h.captureLifecycleChildPIDs(root, 1, "steer-child")
	h.waitLifecycleStreaming(root, child.SessionID, "child running and streaming before steer")
	childSettledBefore := root.journal.countPi(child.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, child.SessionID, map[string]any{
		"type": "steer", "message": "Stop the prior response and include exactly " + childSteerMarker + ".",
	})
	h.waitLifecycleSettlement(root, child.SessionID, childSettledBefore, false, "steered child settlement and open transport")
	result := childCall.wait(h.t, 2*time.Minute)
	h.assertLifecycleReadResult(result, child.SessionID, childSteerMarker)
	h.waitForLifecycleNaturalChildEvents(root, []supervisor.NodeSnapshot{child}, "steered child exact terminal events")
	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{child}, childPIDs, "steered child cleanup")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_STEER_CHILD_ROOT_USABLE_"+h.prefix)
	if got := root.journal.countPi(root.tree.SessionID, "agent_settled", ""); got != rootSettledBefore+3 {
		h.t.Fatalf("steer scenario root settlements = %d, want exactly 3", got-rootSettledBefore)
	}
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) exerciseLifecycleRapidControl() {
	root := h.startLifecycleRoot("rapid-control")
	priorSteerMarker := "KANEDIAS_LIFECYCLE_RAPID_STEER_" + h.prefix
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "prompt", "message": lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_RAPID_ORIGINAL_" + h.prefix),
	})
	h.waitLifecycleStreaming(root, root.tree.SessionID, "root running and streaming before rapid control")
	settledBefore := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "steer", "message": "Stop the prior response and include exactly " + priorSteerMarker + ".",
	})
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{"type": "abort"})
	h.waitLifecycleSettlement(root, root.tree.SessionID, settledBefore, true, "rapid steer-abort settlement with empty pending queue")
	settledAtFollowUp := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	if settledAtFollowUp != settledBefore+1 {
		h.t.Fatalf("rapid steer-abort settlements = %d, want exactly 1", settledAtFollowUp-settledBefore)
	}

	followUpMarker := "KANEDIAS_LIFECYCLE_RAPID_FOLLOW_UP_" + h.prefix
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "prompt", "message": "Reply with exactly " + followUpMarker + ".",
	})
	h.waitLifecycleSettlement(root, root.tree.SessionID, settledAtFollowUp, false, "rapid-control isolated follow-up settlement")
	if got := root.journal.countPi(root.tree.SessionID, "agent_settled", ""); got != settledBefore+2 {
		h.t.Fatalf("rapid-control total settlements = %d, want exactly 2", got-settledBefore)
	}
	text := h.lastAssistantText(root.client, root.tree.SessionID)
	if !strings.Contains(text, followUpMarker) {
		h.t.Fatalf("rapid-control follow-up text %q lacks marker %q", text, followUpMarker)
	}
	if strings.Contains(text, priorSteerMarker) {
		h.t.Fatalf("rapid-control follow-up text %q retained prior steer marker %q", text, priorSteerMarker)
	}
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) sendLifecycleModelPrompt(root *lifecycleRoot, prompt string) {
	response := h.rpc(root.client, root.tree.SessionID, map[string]any{"type": "prompt", "message": prompt})
	if response["success"] != true {
		h.t.Fatalf("model-directed prompt was not accepted: %#v", response)
	}
}

func (h *liveAcceptance) waitForLifecycleModelChildren(root *lifecycleRoot, eventBoundary, settledBefore, count int, description string) supervisor.NodeSnapshot {
	var observed supervisor.NodeSnapshot
	h.poll(8*time.Minute, description, func() bool {
		events := root.journal.snapshot()
		if eventBoundary > len(events) {
			h.t.Fatalf("model event boundary %d exceeds journal length %d", eventBoundary, len(events))
		}
		events = events[eventBoundary:]
		starts := countLifecycleDelegateEvents(events, root.tree.SessionID, "tool_execution_start")
		ends := countLifecycleDelegateEvents(events, root.tree.SessionID, "tool_execution_end")
		if starts > count {
			h.writeLifecycleModelEvidence(root, safeName(description)+"-too-many-tool-calls")
			h.t.Fatalf("%s emitted %d delegate_session starts, want exactly %d", description, starts, count)
		}
		if ends > 0 {
			h.writeLifecycleModelEvidence(root, safeName(description)+"-child-ended-before-snapshot")
			h.t.Fatalf("%s reached a delegate_session tool end before one snapshot showed all %d children (starts=%d)", description, count, starts)
		}
		settled := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
		if settled > settledBefore && starts != count {
			h.writeLifecycleModelEvidence(root, safeName(description)+"-missing-tool-calls")
			h.t.Fatalf("%s settled after %d delegate_session starts, want exactly %d; no retry or direct fallback is permitted", description, starts, count)
		}

		var current supervisor.NodeSnapshot
		if unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &current) != nil || len(current.Children) != count {
			return false
		}
		for _, child := range current.Children {
			if !isControllableChildSnapshot(child, contract.ChildKindRead, contract.ContextFresh) {
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

func countLifecycleDelegateEvents(events []supervisor.EventEnvelope, rootID, eventType string) int {
	count := 0
	for _, event := range events {
		if event.SessionID != rootID || event.Kind != "pi" {
			continue
		}
		var payload struct {
			Type     string `json:"type"`
			ToolName string `json:"toolName"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Type == eventType && payload.ToolName == "delegate_session" {
			count++
		}
	}
	return count
}

func (h *liveAcceptance) waitForLifecycleRootSettlement(root *lifecycleRoot, settledBefore int, description string) {
	h.poll(8*time.Minute, description, func() bool {
		return root.journal.countPi(root.tree.SessionID, "agent_settled", "") > settledBefore
	})
}

func (h *liveAcceptance) waitForLifecycleNaturalChildEvents(root *lifecycleRoot, children []supervisor.NodeSnapshot, description string) {
	childIDs := make([]string, 0, len(children))
	for _, child := range children {
		childIDs = append(childIDs, child.SessionID)
	}
	h.poll(2*time.Minute, description, func() bool {
		events := root.journal.snapshot()
		for _, childID := range childIDs {
			if root.journal.countPi(childID, "agent_settled", "") > 1 {
				h.writeLifecycleModelEvidence(root, safeName(description)+"-duplicate-terminal")
				h.t.Fatalf("child %q emitted a duplicate agent_settled terminal event", childID)
			}
		}
		return validateLifecycleNaturalChildEvents(events, childIDs) == nil
	})
}

func (h *liveAcceptance) writeLifecycleModelEvidence(root *lifecycleRoot, label string) {
	var tree supervisor.NodeSnapshot
	treeErr := unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &tree)
	h.writeJSON(label+"-model-evidence.json", map[string]any{
		"rootModel": root.tree.Model,
		"tree":      tree,
		"treeError": errorString(treeErr),
		"events":    root.journal.snapshot(),
	})
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

func (h *liveAcceptance) assertLifecycleStoppedResult(result lifecycleHTTPResult) contract.Error {
	typed, err := validateLifecycleStoppedResult(result)
	if err != nil {
		h.t.Fatalf("stopped child call did not return the exact cancellation contract: %v; status=%d body=%q", err, result.Status, result.Body)
	}
	return typed
}

func validateLifecycleStoppedResult(result lifecycleHTTPResult) (contract.Error, error) {
	if result.Err != nil {
		return contract.Error{}, fmt.Errorf("transport error instead of typed JSON: %w", result.Err)
	}
	wantStatus := contract.ErrorChildAborted.HTTPStatus()
	if result.Status != wantStatus {
		return contract.Error{}, fmt.Errorf("HTTP status %d, want %d", result.Status, wantStatus)
	}
	var typed contract.Error
	if err := json.Unmarshal(result.Body, &typed); err != nil {
		return contract.Error{}, fmt.Errorf("decode typed JSON error: %w", err)
	}
	if typed.Code != contract.ErrorChildAborted {
		return contract.Error{}, fmt.Errorf("error code %q, want %q", typed.Code, contract.ErrorChildAborted)
	}
	if strings.TrimSpace(typed.Message) == "" {
		return contract.Error{}, fmt.Errorf("error message is empty")
	}
	return typed, nil
}

func validateLifecycleLeafTopology(tree supervisor.NodeSnapshot, childCount int) error {
	if tree.SessionID == "" || tree.ParentSessionID != "" || tree.RootSessionID != tree.SessionID {
		return fmt.Errorf("root identity is not exact: session=%q parent=%q root=%q", tree.SessionID, tree.ParentSessionID, tree.RootSessionID)
	}
	if len(tree.Children) != childCount {
		return fmt.Errorf("direct child count = %d, want %d", len(tree.Children), childCount)
	}
	seen := map[string]struct{}{tree.SessionID: {}}
	for index, child := range tree.Children {
		if child.SessionID == "" || child.ParentSessionID != tree.SessionID || child.RootSessionID != tree.SessionID {
			return fmt.Errorf("child %d is not a direct root-bound session: %#v", index, child)
		}
		if _, duplicate := seen[child.SessionID]; duplicate {
			return fmt.Errorf("duplicate session ID %q", child.SessionID)
		}
		seen[child.SessionID] = struct{}{}
		if len(child.Children) != 0 {
			return fmt.Errorf("child %q has %d descendants, want a leaf", child.SessionID, len(child.Children))
		}
	}
	if len(seen) != childCount+1 {
		return fmt.Errorf("session count = %d, want %d", len(seen), childCount+1)
	}
	return nil
}

func lifecycleActiveReadTask(marker string) string {
	return "Without modifying files, read exactly these bounded repository files: README.md, go.mod, " +
		"internal/supervisor/node.go, internal/supervisor/local.go, internal/supervisor/result.go, " +
		"internal/supervisor/children.go, internal/supervisor/router.go, internal/supervisor/lifecycle.go, " +
		"internal/supervisor/contract/types.go, internal/supervisor/contract/errors.go, " +
		"internal/supervisor/process/spawn.go, and internal/supervisorapi/handler.go. " +
		"For each file identify its lifecycle responsibility, then compare the relationships in a detailed final response containing " + marker + ". " +
		"Do not delegate and do not run sleep, wait, retry, server, watcher, or other long-lived commands."
}

func (h *liveAcceptance) assertLifecycleChildStreaming(root *lifecycleRoot, childID string) {
	state := h.rpc(root.client, childID, map[string]any{"type": "get_state"})
	data, _ := state["data"].(map[string]any)
	if state["success"] != true || data["isStreaming"] != true {
		h.t.Fatalf("child %s was not observably streaming before lifecycle control: %#v", childID, state)
	}
}

func (h *liveAcceptance) captureLifecycleChildPIDs(root *lifecycleRoot, count int, label string) []int {
	pids := directSessionChildPIDs(root.process.cmd.Process.Pid)
	if len(pids) != count {
		h.t.Fatalf("%s direct child supervisor PIDs = %v, want exactly %d", label, pids, count)
	}
	h.writeJSON(label+"-child-supervisor-pids.json", map[string]any{"rootPid": root.process.cmd.Process.Pid, "childPids": pids})
	return pids
}

func (h *liveAcceptance) waitForLifecycleChildrenGone(root *lifecycleRoot, children []supervisor.NodeSnapshot, childPIDs []int, description string) {
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
		for _, pid := range childPIDs {
			if pathExists(filepath.Join("/proc", fmt.Sprint(pid))) {
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
