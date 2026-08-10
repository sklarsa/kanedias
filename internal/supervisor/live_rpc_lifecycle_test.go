//go:build incus

package supervisor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

func TestLiveRPCMixedSiblingLifecycle(t *testing.T) {
	runLifecycleScenario(t, "mixed-siblings", func(h *liveAcceptance) {
		h.exerciseMixedSiblingOutcomes()
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

func TestLifecycleMarkerIsConciseExactAndRunScoped(t *testing.T) {
	cases := []struct {
		name      string
		kind      string
		index     int
		runPrefix string
		want      string
	}{
		{name: "direct single", kind: "DS", index: 0, runPrefix: "e2e-run", want: "KDS_DS_e2e-run"},
		{name: "direct parallel index two", kind: "DP", index: 2, runPrefix: "e2e-run", want: "KDS_DP_2_e2e-run"},
		{name: "direct parallel index zero", kind: "DP", index: 0, runPrefix: "run-9", want: "KDS_DP_0_run-9"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := lifecycleDeterministicMarker(tc.kind, tc.index, tc.runPrefix)
			if got != tc.want {
				t.Fatalf("deterministic marker = %q, want exact %q", got, tc.want)
			}
			if strings.ContainsAny(got, " \t\n") {
				t.Fatalf("deterministic marker %q contains whitespace", got)
			}
			if !strings.HasSuffix(got, tc.runPrefix) {
				t.Fatalf("deterministic marker %q does not retain the complete run prefix %q", got, tc.runPrefix)
			}
		})
	}
}

func TestLifecyclePostAbortProbeExplicitlySupersedesPriorTask(t *testing.T) {
	marker := "KANEDIAS_LIFECYCLE_POST_ABORT_e2e-run"
	prompt := lifecyclePostAbortProbe(marker)

	if count := strings.Count(prompt, marker); count != 1 {
		t.Fatalf("post-abort probe contains marker %d times, want exactly 1: %q", count, prompt)
	}
	for _, required := range []string{
		"aborted",
		"must not be resumed",
		"Do not call any tools",
		"only the marker",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("post-abort probe lacks fixed instruction %q: %q", required, prompt)
		}
	}
}

// lifecyclePostAbortProbe builds the strict post-abort usability probe. It
// states the prior request was aborted and must not be resumed, prohibits tool
// calls, and constrains the response to exactly the byte-exact marker. The
// marker appears exactly once so a strict strings.Contains final-text check
// still proves the exact run identity.
func lifecyclePostAbortProbe(marker string) string {
	return "The previous request was aborted and must not be resumed. Do not call any tools. " +
		"Reply with only the marker " + marker + " and nothing else."
}

// lifecycleDeterministicMarker builds a concise exact run-scoped provenance
// marker for deterministic direct children only. kind is a compact two-letter
// direct code (DS for direct single, DP for direct parallel) and index is the
// unique parallel index. The entire supplied run prefix is retained so strict
// byte-exact output containment still proves the exact run identity.
func lifecycleDeterministicMarker(kind string, index int, runPrefix string) string {
	if kind == "DS" {
		return "KDS_" + kind + "_" + runPrefix
	}
	return fmt.Sprintf("KDS_%s_%d_%s", kind, index, runPrefix)
}

func (h *liveAcceptance) exerciseDeterministicChildren() {
	root := h.startLifecycleRoot("deterministic-children")
	singleMarker := lifecycleDeterministicMarker("DS", 0, h.prefix)
	singleCall := h.startLifecycleChildCall(root, root.tree.SessionID, "direct-single",
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
		parallelMarkers[index] = lifecycleDeterministicMarker("DP", index, h.prefix)
		parallelCalls[index] = h.startLifecycleChildCall(root, root.tree.SessionID,
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
	if text := h.lifecycleLastAssistantText(root, root.tree.SessionID); !strings.Contains(text, singleMarker) {
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
	parallelText := h.lifecycleLastAssistantText(root, root.tree.SessionID)
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
	call := h.startLifecycleChildCall(root, root.tree.SessionID, "active-child-stop",
		lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_CHILD_STOP_UNEXPECTED_SUCCESS_"+h.prefix))

	activeTree := h.waitForLifecycleChildren(root, 1, true, "running child before direct stop")
	child := activeTree.Children[0]
	h.assertLifecycleChildren(root.tree, activeTree.Children)
	childPIDs := h.captureLifecycleChildPIDs(root, 1, "active-child-stop")
	h.poll(2*time.Minute, "child agent_start before direct stop", func() bool {
		return root.journal.countPi(child.SessionID, "agent_start", "") >= 1
	})
	h.assertLifecycleChildStreaming(root, child.SessionID)
	h.captureLifecycleBoundary(root, "pre-control")

	status, err := h.deleteLifecycleSession(root, child.SessionID)
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
	h.captureLifecycleBoundary(root, "post-control")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_CHILD_STOP_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
}

func (h *liveAcceptance) exerciseLifecycleRootEnd() {
	root := h.startLifecycleRoot("root-end")
	calls := make([]*lifecycleChildCall, 3)
	for index := range calls {
		marker := fmt.Sprintf("KANEDIAS_LIFECYCLE_ROOT_END_UNEXPECTED_SUCCESS_%d_%s", index, h.prefix)
		calls[index] = h.startLifecycleChildCall(root, root.tree.SessionID,
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
	root.mu.Lock()
	for _, pid := range descendantProcesses {
		root.childPIDs[pid] = struct{}{}
	}
	root.mu.Unlock()
	h.captureLifecycleBoundary(root, "pre-control")

	status, err := h.deleteLifecycleSession(root, root.tree.SessionID)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("root-end root DELETE = %d, %v", status, err)
	}
	h.finishStoppedLifecycleRoot(root, "root-end DELETE")
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
	h.captureLifecycleBoundary(root, "post-control")
	h.captureLifecycleBoundary(root, "final")
	h.assertLifecycleFinalInvariants(root)
}

func (h *liveAcceptance) exerciseLifecycleInterrupt() {
	root := h.startLifecycleRoot("interrupt")
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "prompt", "message": lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_INTERRUPT_ROOT_UNEXPECTED_" + h.prefix),
	})
	h.waitLifecycleStreaming(root, root.tree.SessionID, "root running and streaming before interrupt")
	h.captureLifecycleBoundary(root, "pre-control")
	rootSettledBefore := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{"type": "abort"})
	h.waitLifecycleSettlement(root, root.tree.SessionID, rootSettledBefore, false, "root interrupt settlement and open transport")
	h.assertLifecycleRootUsableAfterAbort(root, "KANEDIAS_LIFECYCLE_INTERRUPT_ROOT_USABLE_"+h.prefix)

	childCall := h.startLifecycleChildCall(root, root.tree.SessionID, "interrupt-child",
		lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_INTERRUPT_CHILD_UNEXPECTED_"+h.prefix))
	childTree := h.waitForLifecycleChildren(root, 1, true, "running child before interrupt")
	child := childTree.Children[0]
	h.assertLifecycleChildren(root.tree, childTree.Children)
	childPIDs := h.captureLifecycleChildPIDs(root, 1, "interrupt-child")
	h.waitLifecycleStreaming(root, child.SessionID, "child running and streaming before interrupt")
	childSettledBefore := root.journal.countPi(child.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, child.SessionID, map[string]any{"type": "abort"})
	h.assertLifecycleStoppedResult(childCall.wait(h.t, 2*time.Minute))
	h.waitLifecycleSettlementEvent(root, child.SessionID, childSettledBefore, "interrupted child journal settlement")
	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{child}, childPIDs, "interrupted child cleanup")
	h.captureLifecycleBoundary(root, "post-control")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_INTERRUPT_CHILD_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
	h.assertLifecycleSettlementTotals(root, map[string]int{
		root.tree.SessionID: rootSettledBefore + 3,
		child.SessionID:     childSettledBefore + 1,
	}, "interrupt scenario settlement totals")
}

func (h *liveAcceptance) exerciseLifecycleSteer() {
	root := h.startLifecycleRoot("steer")
	rootSteerMarker := "KANEDIAS_LIFECYCLE_STEER_ROOT_" + h.prefix
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "prompt", "message": lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_STEER_ROOT_ORIGINAL_" + h.prefix),
	})
	h.waitLifecycleStreaming(root, root.tree.SessionID, "root running and streaming before steer")
	h.captureLifecycleBoundary(root, "pre-control")
	rootSettledBefore := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "steer", "message": "Stop the prior response and include exactly " + rootSteerMarker + ".",
	})
	h.waitLifecycleSettlement(root, root.tree.SessionID, rootSettledBefore, false, "steered root settlement and open transport")
	if text := h.lifecycleLastAssistantText(root, root.tree.SessionID); !strings.Contains(text, rootSteerMarker) {
		h.t.Fatalf("steered root final text %q lacks marker %q", text, rootSteerMarker)
	}
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_STEER_ROOT_USABLE_"+h.prefix)

	childSteerMarker := "KANEDIAS_LIFECYCLE_STEER_CHILD_" + h.prefix
	childCall := h.startLifecycleChildCall(root, root.tree.SessionID, "steer-child",
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
	result := childCall.wait(h.t, 2*time.Minute)
	h.assertLifecycleReadResult(result, child.SessionID, childSteerMarker)
	h.waitForLifecycleNaturalChildEvents(root, []supervisor.NodeSnapshot{child}, "steered child exact terminal events")
	h.waitForLifecycleChildrenGone(root, []supervisor.NodeSnapshot{child}, childPIDs, "steered child cleanup")
	h.captureLifecycleBoundary(root, "post-control")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_STEER_CHILD_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
	h.assertLifecycleSettlementTotals(root, map[string]int{
		root.tree.SessionID: rootSettledBefore + 3,
		child.SessionID:     childSettledBefore + 1,
	}, "steer scenario settlement totals")
}

func (h *liveAcceptance) exerciseLifecycleRapidControl() {
	root := h.startLifecycleRoot("rapid-control")
	priorSteerMarker := "KANEDIAS_LIFECYCLE_RAPID_STEER_" + h.prefix
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{
		"type": "prompt", "message": lifecycleActiveReadTask("KANEDIAS_LIFECYCLE_RAPID_ORIGINAL_" + h.prefix),
	})
	h.waitLifecycleStreaming(root, root.tree.SessionID, "root running and streaming before rapid control")
	h.captureLifecycleBoundary(root, "pre-control")
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
	text := h.lifecycleLastAssistantText(root, root.tree.SessionID)
	if !strings.Contains(text, followUpMarker) {
		h.t.Fatalf("rapid-control follow-up text %q lacks marker %q", text, followUpMarker)
	}
	if strings.Contains(text, priorSteerMarker) {
		h.t.Fatalf("rapid-control follow-up text %q retained prior steer marker %q", text, priorSteerMarker)
	}
	h.captureLifecycleBoundary(root, "post-control")
	h.stopLifecycleRoot(root)
	h.assertLifecycleSettlementTotals(root, map[string]int{
		root.tree.SessionID: settledBefore + 2,
	}, "rapid-control scenario settlement totals")
}

func (h *liveAcceptance) exerciseMixedSiblingOutcomes() {
	root := h.startLifecycleRoot("mixed-siblings")
	markers := map[string]string{
		"natural": "KANEDIAS_LIFECYCLE_MIXED_NATURAL_" + h.prefix,
		"delete":  "KANEDIAS_LIFECYCLE_MIXED_DELETE_UNEXPECTED_" + h.prefix,
		"abort":   "KANEDIAS_LIFECYCLE_MIXED_ABORT_UNEXPECTED_" + h.prefix,
	}
	calls := map[string]*lifecycleChildCall{
		"natural": h.startLifecycleChildCall(root, root.tree.SessionID, "mixed-natural",
			"Without modifying files, read README.md, go.mod, internal/supervisor/node.go, internal/supervisor/local.go, internal/supervisor/result.go, and internal/supervisor/lifecycle.go. Return a detailed comparison containing exactly "+markers["natural"]+". Do not delegate."),
		"delete": h.startLifecycleChildCall(root, root.tree.SessionID, "mixed-delete",
			lifecycleActiveReadTask(markers["delete"])),
		"abort": h.startLifecycleChildCall(root, root.tree.SessionID, "mixed-abort",
			lifecycleActiveReadTask(markers["abort"])),
	}

	allTree := h.waitForLifecycleChildren(root, 3, true, "three mixed siblings in one running tree snapshot")
	if err := validateLifecycleLeafTopology(allTree, 3); err != nil {
		h.t.Fatalf("mixed sibling topology: %v", err)
	}
	h.assertLifecycleChildren(root.tree, allTree.Children)
	allPIDs := h.captureLifecycleChildPIDs(root, 3, "mixed-siblings")
	childrenByOutcome := h.identifyLifecycleChildrenByMarker(root, allTree.Children, markers)
	naturalChild := childrenByOutcome["natural"]
	deleteChild := childrenByOutcome["delete"]
	abortChild := childrenByOutcome["abort"]

	h.captureLifecycleBoundary(root, "pre-control")
	h.waitLifecycleStreaming(root, deleteChild.SessionID, "mixed DELETE child running and streaming immediately before control")
	status, err := h.deleteLifecycleSession(root, deleteChild.SessionID)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("mixed sibling DELETE = %d, %v", status, err)
	}
	h.waitLifecycleStreaming(root, abortChild.SessionID, "mixed abort child running and streaming immediately before control")
	h.lifecycleRPCCommand(root, abortChild.SessionID, map[string]any{"type": "abort"})

	h.assertLifecycleStoppedResult(calls["delete"].wait(h.t, 2*time.Minute))
	h.waitForLifecycleChildGone(root, deleteChild.SessionID, "mixed DELETE child independent disappearance")
	h.assertLifecycleStoppedResult(calls["abort"].wait(h.t, 2*time.Minute))
	h.waitLifecycleSettlementTotal(root, abortChild.SessionID, 1, "mixed abort child exact drained settlement")
	h.waitForLifecycleChildGone(root, abortChild.SessionID, "mixed abort child independent disappearance")

	naturalResult := calls["natural"].wait(h.t, 8*time.Minute)
	naturalRead := h.assertLifecycleReadResult(naturalResult, naturalChild.SessionID, markers["natural"])
	for _, siblingMarker := range []string{markers["delete"], markers["abort"]} {
		if strings.Contains(naturalRead.Output, siblingMarker) {
			h.t.Fatalf("mixed natural child output was contaminated by sibling marker %q", siblingMarker)
		}
	}
	h.waitForLifecycleNaturalChildEvents(root, []supervisor.NodeSnapshot{naturalChild}, "mixed natural child exact terminal events")
	h.waitLifecycleSettlementTotal(root, naturalChild.SessionID, 1, "mixed natural child exact drained settlement")
	h.waitForLifecycleChildGone(root, naturalChild.SessionID, "mixed natural child independent disappearance")
	root.freezeLifecycleEvents(naturalChild.SessionID, root.journal.snapshot())

	h.waitForLifecycleChildrenGone(root, allTree.Children, allPIDs, "mixed sibling identity-specific cleanup")
	h.captureLifecycleBoundary(root, "post-control")
	h.assertRootUsable(root, "KANEDIAS_LIFECYCLE_MIXED_ROOT_USABLE_"+h.prefix)
	h.stopLifecycleRoot(root)
	h.assertLifecycleSettlementTotals(root, map[string]int{
		root.tree.SessionID:    1,
		naturalChild.SessionID: 1,
		abortChild.SessionID:   1,
	}, "mixed sibling abort/natural exact one-generation settlement totals")
}

func (h *liveAcceptance) identifyLifecycleChildrenByMarker(root *lifecycleRoot, children []supervisor.NodeSnapshot, markers map[string]string) map[string]supervisor.NodeSnapshot {
	identified := make(map[string]supervisor.NodeSnapshot, len(markers))
	for _, child := range children {
		response := h.lifecycleRPCCommand(root, child.SessionID, map[string]any{"type": "get_messages"})
		encoded, err := json.Marshal(response["data"])
		if err != nil {
			h.t.Fatalf("encode get_messages data for child %s: %v", child.SessionID, err)
		}
		matched := ""
		for outcome, marker := range markers {
			if strings.Contains(string(encoded), marker) {
				if matched != "" {
					h.t.Fatalf("child %s messages contain markers for both %s and %s", child.SessionID, matched, outcome)
				}
				matched = outcome
			}
		}
		if matched == "" {
			h.t.Fatalf("child %s messages contain no mixed-sibling task marker", child.SessionID)
		}
		if _, duplicate := identified[matched]; duplicate {
			h.t.Fatalf("multiple children mapped to mixed-sibling marker %s", matched)
		}
		identified[matched] = child
	}
	if len(identified) != len(markers) {
		h.t.Fatalf("identified mixed sibling markers = %d, want %d", len(identified), len(markers))
	}
	return identified
}

func (h *liveAcceptance) waitForLifecycleChildGone(root *lifecycleRoot, sessionID, description string) {
	h.poll(2*time.Minute, description, func() bool {
		var current supervisor.NodeSnapshot
		if unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &current) != nil {
			return false
		}
		if _, present := lifecycleSnapshotByID(current, sessionID); present || !h.sessionsAbsent([]string{sessionID}) {
			return false
		}
		return !pathExists(h.recursiveDescendantSocketPath(sessionID))
	})
}

func (h *liveAcceptance) sendLifecycleModelPrompt(root *lifecycleRoot, prompt string) {
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{"type": "prompt", "message": prompt})
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
	root.mu.Lock()
	for _, pid := range pids {
		root.childPIDs[pid] = struct{}{}
	}
	root.mu.Unlock()
	allDescendants := h.recordLifecycleDescendantPIDs(root, label+"-active-children")
	h.writeJSON(label+"-child-supervisor-pids.json", map[string]any{
		"rootPid": root.process.cmd.Process.Pid, "childPids": pids, "allDescendantPids": allDescendants,
	})
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
