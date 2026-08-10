//go:build incus

package supervisor_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
)

func TestValidateLifecycleModelPolicyRequiresLocalRootAndWorkers(t *testing.T) {
	cfg := config.Config{
		Models: map[string]config.ModelDefinition{
			"local": {Provider: "local-executor", Model: "Qwen3.6-27B-GGUF", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off"},
			"paid":  {Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevels: []string{"high"}, DefaultThinkingLevel: "high"},
		},
		Session: config.SessionDefaults{ModelType: "local", ThinkingLevel: "off"},
		Workers: map[string]config.WorkerDefaults{
			"reviewer": {Description: "review", ModelType: "local", ThinkingLevel: "off"},
		},
	}
	if err := validateLifecycleModelPolicy(cfg, "local-executor", "Qwen3.6-27B-GGUF", "off"); err != nil {
		t.Fatalf("valid local policy: %v", err)
	}

	// A catalog-valid same-provider/model worker at a thinking level the local
	// provider cannot realize must be rejected by name with a thinking mismatch.
	cfg.Models["local"] = config.ModelDefinition{Provider: "local-executor", Model: "Qwen3.6-27B-GGUF", ThinkingLevels: []string{"off", "xhigh"}, DefaultThinkingLevel: "off"}
	cfg.Workers["reviewer"] = config.WorkerDefaults{Description: "review", ModelType: "local", ThinkingLevel: "xhigh"}
	thinkingDrift := validateLifecycleModelPolicy(cfg, "local-executor", "Qwen3.6-27B-GGUF", "off")
	if thinkingDrift == nil || !strings.Contains(thinkingDrift.Error(), "reviewer") || !strings.Contains(thinkingDrift.Error(), "thinking") {
		t.Fatalf("same-provider thinking drift error = %v, want reviewer thinking mismatch", thinkingDrift)
	}

	// A mixed-provider/model worker must be rejected by name.
	cfg.Models["local"] = config.ModelDefinition{Provider: "local-executor", Model: "Qwen3.6-27B-GGUF", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off"}
	worker := cfg.Workers["reviewer"]
	worker.ModelType = "paid"
	worker.ThinkingLevel = "high"
	cfg.Workers["reviewer"] = worker
	if err := validateLifecycleModelPolicy(cfg, "local-executor", "Qwen3.6-27B-GGUF", "off"); err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("mixed worker policy error = %v", err)
	}
}

func TestValidateLifecycleFinalEventsRequiresOrderedPairedGenerations(t *testing.T) {
	// Pi's run boundary is agent_end, so every generation must contain
	// agent_start -> agent_end. agent_settled is an optional post-end
	// confirmation; root generation 1 (positions 0-1) intentionally omits it
	// before root generation 2 starts. The broker order at positions 0-4 is
	// agent_start -> agent_end -> agent_start -> agent_end -> agent_settled.
	events := []supervisor.EventEnvelope{
		lifecyclePiEnvelope(1, "root", `{"type":"agent_start"}`),
		lifecyclePiEnvelope(2, "root", `{"type":"agent_end"}`),
		lifecyclePiEnvelope(3, "child", `{"type":"agent_start"}`),
		lifecyclePiEnvelope(4, "child", `{"type":"agent_end"}`),
		lifecyclePiEnvelope(5, "child", `{"type":"agent_settled"}`),
		lifecyclePiEnvelope(6, "root", `{"type":"agent_start"}`),
		lifecyclePiEnvelope(7, "root", `{"type":"agent_end"}`),
	}
	if err := validateLifecycleFinalEvents(events, nil); err != nil {
		t.Fatalf("valid final events: %v", err)
	}

	for _, test := range []struct {
		name string
		edit func([]supervisor.EventEnvelope) []supervisor.EventEnvelope
	}{
		{name: "broker sequence regression", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[2].Seq = events[1].Seq
			return events
		}},
		{name: "source sequence regression", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].SourceSeq = events[0].SourceSeq
			return events
		}},
		{name: "duplicate source identity", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[4].SessionID = events[0].SessionID
			events[4].SourceSeq = events[0].SourceSeq
			return events
		}},
		{name: "end without start", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[0].Payload = json.RawMessage(`{"type":"agent_end"}`)
			return events
		}},
		{name: "overlapping start before end", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[6].Payload = json.RawMessage(`{"type":"agent_start"}`)
			return events
		}},
		{name: "unclosed start", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			return events[:len(events)-1]
		}},
		{name: "settled while generation open", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[6].Payload = json.RawMessage(`{"type":"agent_settled"}`)
			return events
		}},
		{name: "duplicate settled confirmation", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			return append(events, lifecyclePiEnvelope(8, "child", `{"type":"agent_settled"}`))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cloned := make([]supervisor.EventEnvelope, len(events))
			for index, event := range events {
				cloned[index] = cloneLifecycleEnvelope(event)
			}
			if err := validateLifecycleFinalEvents(test.edit(cloned), nil); err == nil {
				t.Fatal("invalid final events were accepted")
			}
		})
	}
}

func TestValidateLifecycleFinalEventsAdmitsOnlyExplicitExternalCancellation(t *testing.T) {
	// A session whose external cancellation truncated its Pi stream after
	// agent_start must be rejected unless its exact session ID is recorded as
	// externally cancelled. A different allowed ID must not excuse it.
	events := []supervisor.EventEnvelope{
		lifecyclePiEnvelope(1, "cancelled-child", `{"type":"agent_start"}`),
	}
	if err := validateLifecycleFinalEvents(events, nil); err == nil {
		t.Fatal("unclosed generation without cancellation evidence was accepted")
	}
	if err := validateLifecycleFinalEvents(events, map[string]struct{}{"other-session": {}}); err == nil {
		t.Fatal("unclosed generation accepted under a different allowed session")
	}
	if err := validateLifecycleFinalEvents(events, map[string]struct{}{"cancelled-child": {}}); err != nil {
		t.Fatalf("explicitly cancelled unclosed generation was rejected: %v", err)
	}

	// An externally cancelled ID must never excuse an ordering/end/settled
	// violation; only an unclosed generation at EOF is eligible.
	invalid := []supervisor.EventEnvelope{
		lifecyclePiEnvelope(1, "cancelled-child", `{"type":"agent_end"}`),
	}
	if err := validateLifecycleFinalEvents(invalid, map[string]struct{}{"cancelled-child": {}}); err == nil {
		t.Fatal("end-without-start was excused by cancellation evidence")
	}
}

func TestLifecycleExternalCancellationEvidence(t *testing.T) {
	tree := supervisor.NodeSnapshot{SessionID: "root"}
	tree.Children = []supervisor.NodeSnapshot{
		{SessionID: "child-1"},
		{SessionID: "child-2"},
	}

	// Non-root direct DELETE records exactly the target, never its tree.
	if got := lifecycleExternalCancellationIDs("child-1", tree, true); !reflect.DeepEqual(got, []string{"child-1"}) {
		t.Fatalf("direct child cancellation ids = %v, want exactly child-1", got)
	}
	// Root DELETE records the root and every currently projected descendant.
	if got := lifecycleExternalCancellationIDs("root", tree, true); !reflect.DeepEqual(got, []string{"root", "child-1", "child-2"}) {
		t.Fatalf("root cancellation ids = %v, want root plus descendants", got)
	}
	// Root DELETE with a failed snapshot records only the root (fails closed).
	if got := lifecycleExternalCancellationIDs("root", tree, false); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatalf("root cancellation ids with failed tree = %v, want only root", got)
	}
	// Empty target adds nothing.
	if got := lifecycleExternalCancellationIDs("", tree, true); len(got) != 0 {
		t.Fatalf("empty cancellation target added ids: %v", got)
	}
}

func TestValidateLifecycleFinalBoundaryRequiresStoppedUnavailableTree(t *testing.T) {
	valid := lifecycleBoundaryEvidence{Tree: lifecycleTreeEvidence{Available: false, RootStopped: true, RootSocketPresent: false}}
	if err := validateLifecycleFinalBoundary(valid); err != nil {
		t.Fatalf("valid stopped final boundary: %v", err)
	}
	for _, invalid := range []lifecycleBoundaryEvidence{
		{Tree: lifecycleTreeEvidence{Available: true, RootStopped: true}},
		{Tree: lifecycleTreeEvidence{Available: false, RootStopped: true, Tree: &supervisor.NodeSnapshot{SessionID: "stale-root"}}},
		{Tree: lifecycleTreeEvidence{Available: false, RootStopped: false}},
		{Tree: lifecycleTreeEvidence{Available: false, RootStopped: true, RootSocketPresent: true}},
	} {
		if err := validateLifecycleFinalBoundary(invalid); err == nil {
			t.Fatalf("invalid final boundary was accepted: %#v", invalid.Tree)
		}
	}
}

func TestValidateLifecycleSSECaptureRequiresCleanExactData(t *testing.T) {
	event := lifecyclePiEnvelope(1, "root", `{"type":"agent_start"}`)
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "root-events.sse")
	if err := os.WriteFile(path, []byte("event: message\n"+"data: "+string(encoded)+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateLifecycleSSECapture(path, []supervisor.EventEnvelope{event}); err != nil {
		t.Fatalf("valid SSE capture: %v", err)
	}
	if err := validateLifecycleSSECompletion(nil); err != nil {
		t.Fatalf("clean SSE completion: %v", err)
	}
	if err := validateLifecycleSSECompletion(errors.New("truncated stream")); err == nil {
		t.Fatal("SSE scanner error was accepted")
	}
	if err := os.WriteFile(path, []byte("data: {not-json}\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateLifecycleSSECapture(path, nil); err == nil {
		t.Fatal("malformed SSE data envelope was accepted")
	}
	if err := os.WriteFile(path, []byte("data: "+string(encoded)+"\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateLifecycleSSECapture(path, nil); err == nil {
		t.Fatal("SSE capture/journal mismatch was accepted")
	}
}

func TestValidateLifecycleLastAssistantTextRequiresExactTypedSuccess(t *testing.T) {
	valid := map[string]any{
		"type": "response", "command": "get_last_assistant_text", "success": true,
		"data": map[string]any{"text": "final text"},
	}
	if text, err := validateLifecycleLastAssistantText(valid); err != nil || text != "final text" {
		t.Fatalf("valid final assistant text = %q, %v", text, err)
	}
	for _, response := range []map[string]any{
		{"type": "event", "command": "get_last_assistant_text", "success": true, "data": map[string]any{"text": "x"}},
		{"type": "response", "command": "get_state", "success": true, "data": map[string]any{"text": "x"}},
		{"type": "response", "command": "get_last_assistant_text", "success": false, "data": map[string]any{"text": "x"}},
		{"type": "response", "command": "get_last_assistant_text", "success": true, "data": map[string]any{}},
		{"type": "response", "command": "get_last_assistant_text", "success": true, "data": map[string]any{"text": 7}},
	} {
		if _, err := validateLifecycleLastAssistantText(response); err == nil {
			t.Fatalf("invalid final assistant response was accepted: %#v", response)
		}
	}
}

func TestLifecycleActionsRemainNormalizedAndPrivate(t *testing.T) {
	action := lifecycleAction{Sequence: 1, Operation: "abort", TargetSessionID: "child", Outcome: "accepted", HTTPStatus: http.StatusAccepted, ErrorCode: contract.ErrorChildAborted}
	encoded, err := json.Marshal(action)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{"body", "url", "credential", "rawError", "modelOutput", "secret-value", "https://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("normalized lifecycle action contains forbidden data class %q: %s", forbidden, text)
		}
	}
}

func TestValidateLifecycleSettlementTotalsRequiresExactAbsoluteCountsAfterDrain(t *testing.T) {
	events := []supervisor.EventEnvelope{
		lifecyclePiEnvelope(1, "root", `{"type":"agent_settled"}`),
		lifecyclePiEnvelope(2, "child", `{"type":"agent_settled"}`),
		lifecyclePiEnvelope(3, "root", `{"type":"agent_settled"}`),
		lifecyclePiEnvelope(4, "root", `{"type":"message_end"}`),
		{Seq: 5, SessionID: "root", Kind: "supervisor", Payload: json.RawMessage(`{"type":"agent_settled"}`)},
	}
	if err := validateLifecycleSettlementTotals(events, map[string]int{"root": 2, "child": 1}); err != nil {
		t.Fatalf("valid drained settlement totals: %v", err)
	}

	withBufferedDuplicate := append(append([]supervisor.EventEnvelope(nil), events...),
		lifecyclePiEnvelope(6, "root", `{"type":"agent_settled"}`))
	if err := validateLifecycleSettlementTotals(withBufferedDuplicate, map[string]int{"root": 2, "child": 1}); err == nil {
		t.Fatal("buffered duplicate settlement was accepted")
	}
	if err := validateLifecycleSettlementTotals(events, map[string]int{"root": 2, "child": 2}); err == nil {
		t.Fatal("missing settlement was accepted")
	}
	if err := validateLifecycleSettlementTotals(events, map[string]int{"": 0}); err == nil {
		t.Fatal("empty expected session ID was accepted")
	}
	if err := validateLifecycleSettlementTotals(events, map[string]int{"root": -1}); err == nil {
		t.Fatal("negative expected total was accepted")
	}
}

func TestLifecycleEventJournalPreservesOrderAndSupportsRepeatedQueries(t *testing.T) {
	input := make(chan supervisor.EventEnvelope, 3)
	stream := &sseCapture{events: input}
	journal := newLifecycleEventJournal(stream)
	input <- supervisor.EventEnvelope{Seq: 1, SessionID: "root", Kind: "pi", Payload: json.RawMessage(`{"type":"agent_start"}`)}
	input <- supervisor.EventEnvelope{Seq: 2, SessionID: "root", Kind: "pi", Payload: json.RawMessage(`{"type":"tool_execution_start","toolName":"delegate_session"}`)}
	close(input)
	<-journal.done

	first := journal.snapshot()
	second := journal.snapshot()
	if len(first) != 2 || len(second) != 2 || first[0].Seq != 1 || first[1].Seq != 2 {
		t.Fatalf("journal snapshots = %#v / %#v", first, second)
	}
	first[0].SessionID = "mutated"
	if journal.snapshot()[0].SessionID != "root" {
		t.Fatal("snapshot aliases retained journal state")
	}
	if got := journal.countPi("root", "tool_execution_start", "delegate_session"); got != 1 {
		t.Fatalf("delegate_session starts = %d, want 1", got)
	}
}

func TestValidateLifecycleModelToolEventsRequiresExactParallelSuccess(t *testing.T) {
	markers := []string{"MARKER_ONE", "MARKER_TWO", "MARKER_THREE"}
	childIDs := []string{"child-one", "child-two", "child-three"}
	events := []supervisor.EventEnvelope{
		lifecyclePiEnvelope(1, "root", `{"type":"tool_execution_start","toolCallId":"one","toolName":"delegate_session","args":{"workerType":"reviewer","kind":"read","context":"fresh","task":"return MARKER_ONE"}}`),
		lifecyclePiEnvelope(2, "root", `{"type":"tool_execution_start","toolCallId":"two","toolName":"delegate_session","args":{"workerType":"reviewer","kind":"read","context":"fresh","task":"return MARKER_TWO"}}`),
		lifecyclePiEnvelope(3, "root", `{"type":"tool_execution_start","toolCallId":"three","toolName":"delegate_session","args":{"workerType":"reviewer","kind":"read","context":"fresh","task":"return MARKER_THREE"}}`),
		lifecyclePiEnvelope(4, "root", `{"type":"tool_execution_end","toolCallId":"two","toolName":"delegate_session","isError":false,"result":{"content":[{"type":"text","text":"MARKER_TWO"}],"details":{"kind":"read","workerType":"reviewer","sessionId":"child-two","output":"MARKER_TWO"}}}`),
		lifecyclePiEnvelope(5, "root", `{"type":"tool_execution_end","toolCallId":"one","toolName":"delegate_session","isError":false,"result":{"content":[{"type":"text","text":"MARKER_ONE"}],"details":{"kind":"read","workerType":"reviewer","sessionId":"child-one","output":"MARKER_ONE"}}}`),
		lifecyclePiEnvelope(6, "root", `{"type":"tool_execution_end","toolCallId":"three","toolName":"delegate_session","isError":false,"result":{"content":[{"type":"text","text":"MARKER_THREE"}],"details":{"kind":"read","workerType":"reviewer","sessionId":"child-three","output":"MARKER_THREE"}}}`),
	}
	if err := validateLifecycleModelToolEvents(events, "root", markers, childIDs, true); err != nil {
		t.Fatalf("valid parallel model tools: %v", err)
	}
	withUnrelatedEnd := append(append([]supervisor.EventEnvelope(nil), events...),
		lifecyclePiEnvelope(7, "root", `{"type":"tool_execution_end","toolCallId":"unrelated","toolName":"read","isError":false}`))
	if err := validateLifecycleModelToolEvents(withUnrelatedEnd, "root", markers, childIDs, true); err != nil {
		t.Fatalf("unrelated tool terminal was not ignored: %v", err)
	}

	for _, test := range []struct {
		name string
		edit func([]supervisor.EventEnvelope) []supervisor.EventEnvelope
	}{
		{name: "fewer starts", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope { return events[1:] }},
		{name: "sequential execution", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[1], events[3] = events[3], events[1]
			return events
		}},
		{name: "duplicate tool call ID", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[1].Payload = json.RawMessage(strings.ReplaceAll(string(events[1].Payload), `"two"`, `"one"`))
			return events
		}},
		{name: "wrong arguments", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[0].Payload = json.RawMessage(strings.Replace(string(events[0].Payload), `"reviewer"`, `"worker"`, 1))
			return events
		}},
		{name: "error result", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].Payload = json.RawMessage(strings.Replace(string(events[3].Payload), `"isError":false`, `"isError":true`, 1))
			return events
		}},
		{name: "duplicate terminal", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			return append(events, cloneLifecycleEnvelope(events[3]))
		}},
		{name: "unmatched delegate terminal", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			return append(events, lifecyclePiEnvelope(7, "root", `{"type":"tool_execution_end","toolCallId":"extra","toolName":"delegate_session","isError":false}`))
		}},
		{name: "conflicting tool name", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].Payload = json.RawMessage(strings.Replace(string(events[3].Payload), `"toolName":"delegate_session"`, `"toolName":"read"`, 1))
			return events
		}},
		{name: "absent details", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].Payload = json.RawMessage(strings.Replace(string(events[3].Payload), `,"details":{"kind":"read","workerType":"reviewer","sessionId":"child-two","output":"MARKER_TWO"}`, "", 1))
			return events
		}},
		{name: "duplicate result session ID", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].Payload = json.RawMessage(strings.Replace(string(events[3].Payload), "child-two", "child-one", 1))
			return events
		}},
		{name: "unobserved result session ID", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].Payload = json.RawMessage(strings.Replace(string(events[3].Payload), "child-two", "child-other", 1))
			return events
		}},
		{name: "incorrect typed kind", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].Payload = json.RawMessage(strings.Replace(string(events[3].Payload), `"kind":"read"`, `"kind":"write"`, 1))
			return events
		}},
		{name: "missing typed worker identity", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].Payload = json.RawMessage(strings.Replace(string(events[3].Payload), `"workerType":"reviewer"`, `"workerType":""`, 1))
			return events
		}},
		{name: "missing typed session identity", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[3].Payload = json.RawMessage(strings.Replace(string(events[3].Payload), `"sessionId":"child-two"`, `"sessionId":""`, 1))
			return events
		}},
		{name: "missing result marker", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[5].Payload = json.RawMessage(strings.ReplaceAll(string(events[5].Payload), "MARKER_THREE", "missing"))
			return events
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cloned := make([]supervisor.EventEnvelope, len(events))
			for index, event := range events {
				cloned[index] = cloneLifecycleEnvelope(event)
			}
			if err := validateLifecycleModelToolEvents(test.edit(cloned), "root", markers, childIDs, true); err == nil {
				t.Fatal("invalid model tool events were accepted")
			}
		})
	}
}

func TestValidateLifecycleNaturalChildEventsRejectsDuplicateTerminalEvent(t *testing.T) {
	events := []supervisor.EventEnvelope{
		lifecyclePiEnvelope(1, "child", `{"type":"message_end","message":{"role":"assistant","stopReason":"stop"}}`),
		lifecyclePiEnvelope(2, "child", `{"type":"agent_settled"}`),
	}
	if err := validateLifecycleNaturalChildEvents(events, []string{"child"}); err != nil {
		t.Fatalf("valid natural child events: %v", err)
	}
	events = append(events, lifecyclePiEnvelope(3, "child", `{"type":"agent_settled"}`))
	if err := validateLifecycleNaturalChildEvents(events, []string{"child"}); err == nil {
		t.Fatal("duplicate child terminal event was accepted")
	}
}

func lifecyclePiEnvelope(seq uint64, sessionID, payload string) supervisor.EventEnvelope {
	return supervisor.EventEnvelope{Seq: seq, SessionID: sessionID, SourceSeq: seq, Kind: "pi", Payload: json.RawMessage(payload)}
}

// validateLifecycleModelPolicy resolves the whole session model policy and
// requires the root and every configured worker to resolve to one exact
// provider/model/thinking triple. Failures identify the worker by name so a mixed
// live configuration is diagnosed against the intended local model rather than a
// silent provider or reasoning-effort drift.
func validateLifecycleModelPolicy(cfg config.Config, provider, model, thinkingLevel string) error {
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		return err
	}
	if policy.Root.Provider != provider || policy.Root.Model != model || policy.Root.ThinkingLevel != thinkingLevel {
		return fmt.Errorf("root model policy %s/%s/%s, want %s/%s/%s", policy.Root.Provider, policy.Root.Model, policy.Root.ThinkingLevel, provider, model, thinkingLevel)
	}
	for _, name := range policy.WorkerNames() {
		worker := policy.Workers[name]
		if worker.Provider != provider || worker.Model != model || worker.ThinkingLevel != thinkingLevel {
			return fmt.Errorf("worker %q thinking model policy %s/%s/%s, want %s/%s/%s", name, worker.Provider, worker.Model, worker.ThinkingLevel, provider, model, thinkingLevel)
		}
	}
	return nil
}

// lifecycleHTTPResult carries the terminal outcome of an asynchronous child
// call so a scenario can require an exact status/body/error after the fact.
type lifecycleHTTPResult struct {
	Status int
	Body   []byte
	Err    error
}

// lifecycleChildCall tracks one asynchronous child-create POST so the caller
// can block on its terminal result with an explicit deadline.
type lifecycleChildCall struct {
	label string
	done  chan lifecycleHTTPResult
}

// lifecycleAction is deliberately normalized: it records only control class,
// target identity, public status/code, and outcome. It has no field capable of
// retaining a request body, URL, credential, raw error, or model output.
type lifecycleAction struct {
	Sequence        int                `json:"sequence"`
	Operation       string             `json:"operation"`
	TargetSessionID string             `json:"targetSessionId"`
	Outcome         string             `json:"outcome"`
	HTTPStatus      int                `json:"httpStatus,omitempty"`
	ErrorCode       contract.ErrorCode `json:"errorCode,omitempty"`
}

type lifecycleTreeEvidence struct {
	Available         bool                     `json:"available"`
	RootStopped       bool                     `json:"rootStopped"`
	RootSocketPresent bool                     `json:"rootSocketPresent"`
	Tree              *supervisor.NodeSnapshot `json:"tree,omitempty"`
}

type lifecycleBoundaryEvidence struct {
	Tree      lifecycleTreeEvidence
	Resources resourceSnapshot
}

// lifecycleEventJournal is the sole consumer of a stream's event channel. It
// appends cloned envelopes under a mutex, closes done when the stream closes,
// and returns deep copies from snapshot so queries never alias journal state.
type lifecycleEventJournal struct {
	mu     sync.Mutex
	events []supervisor.EventEnvelope
	done   chan struct{}
}

// lifecycleRoot bundles the owned root process, socket, faithful tree, HTTP
// client, SSE capture, and the event journal started as soon as the root is up.
type lifecycleRoot struct {
	process   *acceptanceProcess
	socket    string
	tree      supervisor.NodeSnapshot
	client    *http.Client
	stream    *sseCapture
	journal   *lifecycleEventJournal
	stalled   net.Conn
	eventPath string

	mu                sync.Mutex
	actions           []lifecycleAction
	childPIDs         map[int]struct{}
	preControl        *lifecycleBoundaryEvidence
	postControl       *lifecycleBoundaryEvidence
	final             *lifecycleBoundaryEvidence
	immutableEvents   map[string][]supervisor.EventEnvelope
	externalCancelled map[string]struct{}
}

// newLifecycleEventJournal starts a goroutine that drains stream.events as its
// only consumer, cloning every envelope into an ordered journal. done closes
// once the stream closes.
func newLifecycleEventJournal(stream *sseCapture) *lifecycleEventJournal {
	journal := &lifecycleEventJournal{done: make(chan struct{})}
	go func() {
		for event := range stream.events {
			journal.mu.Lock()
			journal.events = append(journal.events, cloneLifecycleEnvelope(event))
			journal.mu.Unlock()
		}
		close(journal.done)
	}()
	return journal
}

// snapshot returns an ordered deep copy of every journaled event.
func (journal *lifecycleEventJournal) snapshot() []supervisor.EventEnvelope {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	out := make([]supervisor.EventEnvelope, len(journal.events))
	for index, event := range journal.events {
		out[index] = cloneLifecycleEnvelope(event)
	}
	return out
}

// countPi counts pi events for a session matching a payload type and,
// optionally, a tool name. Payload parsing uses only type and toolName.
func (journal *lifecycleEventJournal) countPi(sessionID, eventType, toolName string) int {
	count := 0
	for _, event := range journal.snapshot() {
		if event.SessionID != sessionID || event.Kind != "pi" {
			continue
		}
		var payload struct {
			Type     string `json:"type"`
			ToolName string `json:"toolName"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Type == eventType && (toolName == "" || payload.ToolName == toolName) {
			count++
		}
	}
	return count
}

// validateLifecycleFinalEvents requires strict broker ordering, strict
// per-session source ordering, unique source identities, and non-overlapping
// agent_start/agent_end generation boundaries. agent_settled is an optional
// post-end confirmation: at most one is accepted per ended generation, and only
// when no generation is open. A session recorded in externallyCancelled is the
// only case permitted to end the stream with an unclosed generation, because an
// external DELETE cancels descendant event forwarding after agent_start. All
// ordering, uniqueness, end-without-start, overlap, and settled-state checks
// remain strict even for externally cancelled sessions.
func validateLifecycleFinalEvents(events []supervisor.EventEnvelope, externallyCancelled map[string]struct{}) error {
	var brokerSeq uint64
	sourceSeq := make(map[string]uint64)
	seenSources := make(map[string]struct{}, len(events))
	openGeneration := make(map[string]bool)
	settlementEligible := make(map[string]bool)
	for index, event := range events {
		if event.Seq == 0 || (index > 0 && event.Seq <= brokerSeq) {
			return fmt.Errorf("broker sequence at position %d = %d after %d", index, event.Seq, brokerSeq)
		}
		brokerSeq = event.Seq
		if strings.TrimSpace(event.SessionID) == "" || event.SourceSeq == 0 {
			return fmt.Errorf("event %d has empty source identity: session=%q sourceSeq=%d", event.Seq, event.SessionID, event.SourceSeq)
		}
		if prior := sourceSeq[event.SessionID]; prior != 0 && event.SourceSeq <= prior {
			return fmt.Errorf("session %q source sequence %d is not after %d", event.SessionID, event.SourceSeq, prior)
		}
		sourceSeq[event.SessionID] = event.SourceSeq
		key := fmt.Sprintf("%s\x00%d", event.SessionID, event.SourceSeq)
		if _, duplicate := seenSources[key]; duplicate {
			return fmt.Errorf("duplicate event source identity for session %q sequence %d", event.SessionID, event.SourceSeq)
		}
		seenSources[key] = struct{}{}
		if event.Kind != "pi" {
			continue
		}
		var payload struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		switch payload.Type {
		case "agent_start":
			if openGeneration[event.SessionID] {
				return fmt.Errorf("session %q started a generation while its prior generation remained open", event.SessionID)
			}
			openGeneration[event.SessionID] = true
			// A new generation clears any unconsumed settlement confirmation
			// eligibility from an earlier ended generation.
			settlementEligible[event.SessionID] = false
		case "agent_end":
			if !openGeneration[event.SessionID] {
				return fmt.Errorf("session %q ended a generation without an open start", event.SessionID)
			}
			openGeneration[event.SessionID] = false
			settlementEligible[event.SessionID] = true
		case "agent_settled":
			if openGeneration[event.SessionID] {
				return fmt.Errorf("session %q settled while its generation was open", event.SessionID)
			}
			if !settlementEligible[event.SessionID] {
				return fmt.Errorf("session %q settled without an eligible ended generation", event.SessionID)
			}
			settlementEligible[event.SessionID] = false
		}
	}
	for sessionID, open := range openGeneration {
		if open {
			if _, allowed := externallyCancelled[sessionID]; allowed {
				continue
			}
			return fmt.Errorf("session %q has an unclosed started generation", sessionID)
		}
	}
	return nil
}

func validateLifecycleFinalBoundary(final lifecycleBoundaryEvidence) error {
	if final.Tree.Available || !final.Tree.RootStopped || final.Tree.RootSocketPresent || final.Tree.Tree != nil {
		return fmt.Errorf("final tree boundary is not a truthful stopped/unavailable root: %#v", final.Tree)
	}
	return nil
}

func validateLifecycleSSECompletion(captureErr error) error {
	if captureErr != nil {
		return fmt.Errorf("lifecycle SSE capture did not complete cleanly: %w", captureErr)
	}
	return nil
}

func validateLifecycleSSECapture(path string, journal []supervisor.EventEnvelope) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open lifecycle SSE capture: %w", err)
	}
	defer file.Close()

	var captured []supervisor.EventEnvelope
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var event supervisor.EventEnvelope
		if data == "" || json.Unmarshal([]byte(data), &event) != nil {
			return fmt.Errorf("lifecycle SSE data line %d is not a valid event envelope", lineNumber)
		}
		captured = append(captured, event)
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read lifecycle SSE capture: %w", err)
	}
	if len(captured) != len(journal) {
		return fmt.Errorf("lifecycle SSE envelopes = %d, journal events = %d", len(captured), len(journal))
	}
	for index := range captured {
		left, right := captured[index], journal[index]
		if left.Seq != right.Seq || left.SessionID != right.SessionID || left.SourceSeq != right.SourceSeq ||
			left.Kind != right.Kind || string(left.Payload) != string(right.Payload) {
			return fmt.Errorf("lifecycle SSE envelope %d does not match journal", index)
		}
	}
	return nil
}

func validateLifecycleLastAssistantText(response map[string]any) (string, error) {
	encoded, err := json.Marshal(response)
	if err != nil {
		return "", fmt.Errorf("encode get_last_assistant_text response: %w", err)
	}
	var typed struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Success bool   `json:"success"`
		Data    *struct {
			Text *string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(encoded, &typed); err != nil {
		return "", fmt.Errorf("decode get_last_assistant_text response: %w", err)
	}
	if typed.Type != "response" || typed.Command != "get_last_assistant_text" || !typed.Success || typed.Data == nil || typed.Data.Text == nil {
		return "", fmt.Errorf("get_last_assistant_text response envelope is not exact")
	}
	return *typed.Data.Text, nil
}

// validateLifecycleSettlementTotals verifies absolute per-session settlement
// totals from a fully drained event snapshot. Callers include their scenario
// baselines in expected so a buffered duplicate cannot become a later baseline.
func validateLifecycleSettlementTotals(events []supervisor.EventEnvelope, expected map[string]int) error {
	counts := make(map[string]int, len(expected))
	for sessionID, want := range expected {
		if strings.TrimSpace(sessionID) == "" {
			return fmt.Errorf("expected settlement session ID is empty")
		}
		if want < 0 {
			return fmt.Errorf("expected settlement total for %q is negative: %d", sessionID, want)
		}
		counts[sessionID] = 0
	}
	for _, event := range events {
		if event.Kind != "pi" {
			continue
		}
		if _, tracked := counts[event.SessionID]; !tracked {
			continue
		}
		var payload struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Type == "agent_settled" {
			counts[event.SessionID]++
		}
	}
	for sessionID, want := range expected {
		if got := counts[sessionID]; got != want {
			return fmt.Errorf("session %q agent_settled total = %d, want exactly %d", sessionID, got, want)
		}
	}
	return nil
}

// validateLifecycleModelToolEvents accepts only the requested root
// delegate_session calls, exact reviewer/read/fresh arguments, one successful
// typed child result per observed session, and (when requested) starts for the
// whole batch before any matching tool ends.
func validateLifecycleModelToolEvents(events []supervisor.EventEnvelope, rootID string, markers, observedChildIDs []string, requireParallel bool) error {
	type toolEvent struct {
		position int
		payload  struct {
			Type       string          `json:"type"`
			ToolCallID string          `json:"toolCallId"`
			ToolName   string          `json:"toolName"`
			Args       json.RawMessage `json:"args"`
			IsError    *bool           `json:"isError"`
			Result     json.RawMessage `json:"result"`
		}
	}
	var starts, ends []toolEvent
	for position, event := range events {
		if event.SessionID != rootID || event.Kind != "pi" {
			continue
		}
		candidate := toolEvent{position: position}
		if err := json.Unmarshal(event.Payload, &candidate.payload); err != nil {
			continue
		}
		switch candidate.payload.Type {
		case "tool_execution_start":
			if candidate.payload.ToolName == "delegate_session" {
				starts = append(starts, candidate)
			}
		case "tool_execution_end":
			if candidate.payload.ToolName == "delegate_session" || candidate.payload.ToolCallID != "" {
				ends = append(ends, candidate)
			}
		}
	}
	if len(starts) != len(markers) {
		return fmt.Errorf("delegate_session starts = %d, want exactly %d", len(starts), len(markers))
	}

	startsByID := make(map[string]toolEvent, len(starts))
	markerByID := make(map[string]string, len(starts))
	for _, start := range starts {
		if start.payload.ToolCallID == "" {
			return fmt.Errorf("delegate_session start has empty toolCallId")
		}
		if _, duplicate := startsByID[start.payload.ToolCallID]; duplicate {
			return fmt.Errorf("duplicate delegate_session toolCallId %q", start.payload.ToolCallID)
		}
		var args struct {
			WorkerType string `json:"workerType"`
			Kind       string `json:"kind"`
			Context    string `json:"context"`
			Task       string `json:"task"`
		}
		if err := json.Unmarshal(start.payload.Args, &args); err != nil {
			return fmt.Errorf("decode delegate_session %q arguments: %w", start.payload.ToolCallID, err)
		}
		if args.WorkerType != "reviewer" || args.Kind != "read" || args.Context != "fresh" || strings.TrimSpace(args.Task) == "" {
			return fmt.Errorf("delegate_session %q arguments are not exact reviewer/read/fresh with a task", start.payload.ToolCallID)
		}
		matched := ""
		for _, marker := range markers {
			if strings.Contains(args.Task, marker) {
				if matched != "" {
					return fmt.Errorf("delegate_session %q task contains multiple requested markers", start.payload.ToolCallID)
				}
				matched = marker
			}
		}
		if matched == "" {
			return fmt.Errorf("delegate_session %q task contains no requested marker", start.payload.ToolCallID)
		}
		for _, existing := range markerByID {
			if existing == matched {
				return fmt.Errorf("requested marker %q used by multiple delegate_session calls", matched)
			}
		}
		startsByID[start.payload.ToolCallID] = start
		markerByID[start.payload.ToolCallID] = matched
	}

	if len(observedChildIDs) != len(markers) {
		return fmt.Errorf("observed child IDs = %d, want exactly %d", len(observedChildIDs), len(markers))
	}
	observedSessions := make(map[string]struct{}, len(observedChildIDs))
	for _, childID := range observedChildIDs {
		if childID == "" {
			return fmt.Errorf("observed child session ID is empty")
		}
		if _, duplicate := observedSessions[childID]; duplicate {
			return fmt.Errorf("duplicate observed child session ID %q", childID)
		}
		observedSessions[childID] = struct{}{}
	}

	endsByID := make(map[string][]toolEvent, len(starts))
	for _, end := range ends {
		_, expected := startsByID[end.payload.ToolCallID]
		if end.payload.ToolName == "delegate_session" && !expected {
			return fmt.Errorf("unmatched delegate_session terminal toolCallId %q", end.payload.ToolCallID)
		}
		if !expected {
			continue
		}
		if end.payload.ToolName != "" && end.payload.ToolName != "delegate_session" {
			return fmt.Errorf("delegate_session %q terminal has conflicting tool name %q", end.payload.ToolCallID, end.payload.ToolName)
		}
		endsByID[end.payload.ToolCallID] = append(endsByID[end.payload.ToolCallID], end)
	}
	firstEnd := len(events)
	lastStart := -1
	resultSessions := make(map[string]string, len(starts))
	for id, start := range startsByID {
		if start.position > lastStart {
			lastStart = start.position
		}
		matchingEnds := endsByID[id]
		if len(matchingEnds) != 1 {
			return fmt.Errorf("delegate_session %q terminal tool events = %d, want exactly 1", id, len(matchingEnds))
		}
		end := matchingEnds[0]
		if end.position < firstEnd {
			firstEnd = end.position
		}
		if end.payload.IsError == nil || *end.payload.IsError {
			return fmt.Errorf("delegate_session %q did not end with explicit success", id)
		}
		var result struct {
			Details *contract.ReadChildResult `json:"details"`
		}
		if err := json.Unmarshal(end.payload.Result, &result); err != nil {
			return fmt.Errorf("decode delegate_session %q result: %w", id, err)
		}
		if result.Details == nil {
			return fmt.Errorf("delegate_session %q result has no typed details", id)
		}
		details := *result.Details
		if details.Kind != contract.ChildKindRead || details.WorkerType != "reviewer" || details.SessionID == "" {
			return fmt.Errorf("delegate_session %q result has incorrect typed read-child identity: %#v", id, details)
		}
		if _, observed := observedSessions[details.SessionID]; !observed {
			return fmt.Errorf("delegate_session %q result session %q was not observed", id, details.SessionID)
		}
		if prior, duplicate := resultSessions[details.SessionID]; duplicate {
			return fmt.Errorf("delegate_session calls %q and %q returned duplicate child session %q", prior, id, details.SessionID)
		}
		if !strings.Contains(details.Output, markerByID[id]) {
			return fmt.Errorf("delegate_session %q typed output lacks marker %q", id, markerByID[id])
		}
		resultSessions[details.SessionID] = id
	}
	for childID := range observedSessions {
		if _, matched := resultSessions[childID]; !matched {
			return fmt.Errorf("observed child session %q has no delegate_session result", childID)
		}
	}
	if requireParallel && lastStart >= firstEnd {
		return fmt.Errorf("delegate_session calls were not one parallel batch: last start position %d, first end position %d", lastStart, firstEnd)
	}
	return nil
}

// validateLifecycleNaturalChildEvents requires one successful assistant
// message terminal and one (non-duplicated) settlement for every observed child.
func validateLifecycleNaturalChildEvents(events []supervisor.EventEnvelope, childIDs []string) error {
	for _, childID := range childIDs {
		settled := 0
		naturalMessages := 0
		for _, event := range events {
			if event.SessionID != childID || event.Kind != "pi" {
				continue
			}
			var payload struct {
				Type    string `json:"type"`
				Message struct {
					Role       string `json:"role"`
					StopReason string `json:"stopReason"`
				} `json:"message"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if payload.Type == "agent_settled" {
				settled++
			}
			if payload.Type == "message_end" && payload.Message.Role == "assistant" && payload.Message.StopReason == "stop" {
				naturalMessages++
			}
		}
		if settled != 1 {
			return fmt.Errorf("child %q agent_settled events = %d, want exactly 1", childID, settled)
		}
		if naturalMessages != 1 {
			return fmt.Errorf("child %q natural assistant message terminals = %d, want exactly 1", childID, naturalMessages)
		}
	}
	return nil
}

func cloneLifecycleEnvelope(event supervisor.EventEnvelope) supervisor.EventEnvelope {
	event.Payload = append(json.RawMessage(nil), event.Payload...)
	return event
}

// prepareLifecycleConfig redirects the authorized live configuration onto the
// exact local provider/model for every worker and assigns the generated
// run-local config to the harness so the owned root and proxy use it.
func (h *liveAcceptance) prepareLifecycleConfig() {
	if strings.TrimSpace(os.Getenv("KANEDIAS_E2E_WORKER_PROVIDER")) != "local-executor" {
		h.t.Fatalf("lifecycle suite requires KANEDIAS_E2E_WORKER_PROVIDER=local-executor")
	}
	if strings.TrimSpace(os.Getenv("KANEDIAS_E2E_WORKER_MODEL")) != "Qwen3.6-27B-GGUF" {
		h.t.Fatalf("lifecycle suite requires KANEDIAS_E2E_WORKER_MODEL=Qwen3.6-27B-GGUF")
	}

	rootSocketDir := h.shortSocketDir()
	h.managedSocketDir = rootSocketDir
	sessionLogDir := filepath.Join(h.runDir, "lifecycle-logs")
	for _, dir := range []string{rootSocketDir, sessionLogDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			h.t.Fatal(err)
		}
	}

	// writeManagedConfig must run before h.configPath is replaced so it merges
	// the run-local server/event settings into the authorized config.
	managedConfig := h.writeManagedConfig(rootSocketDir, sessionLogDir)
	managedCfg, err := config.Load(managedConfig)
	if err != nil {
		h.t.Fatalf("load lifecycle managed configuration: %v", err)
	}
	if err := managedCfg.ValidateSupervisor(); err != nil {
		h.t.Fatalf("validate lifecycle managed configuration: %v", err)
	}
	h.configPath = managedConfig
	h.cfg = managedCfg
	if err := validateLifecycleModelPolicy(h.cfg, "local-executor", "Qwen3.6-27B-GGUF", "off"); err != nil {
		h.t.Fatalf("lifecycle local model policy: %v", err)
	}
}

// startLifecycleChildCall tracks a child-create POST in h.async and returns a
// handle the caller can wait on for the terminal lifecycleHTTPResult. A
// JSON-safe artifact records status, body text, and the serialized error.
func (h *liveAcceptance) startLifecycleChildCall(root *lifecycleRoot, parentID, label, task string) *lifecycleChildCall {
	request := map[string]any{
		"workerType": "reviewer",
		"kind":       "read",
		"context":    "fresh",
		"task":       task,
	}
	call := &lifecycleChildCall{label: label, done: make(chan lifecycleHTTPResult, 1)}
	h.async.Add(1)
	go func() {
		defer h.async.Done()
		status, body, err := unixRequest(root.client, http.MethodPost,
			"/v1/sessions/"+url.PathEscape(parentID)+"/children", request)
		h.writeJSON(label+"-child-call-result.json", map[string]any{
			"status": status, "body": string(body), "error": errorString(err),
		})
		outcome := "completed"
		if err != nil {
			outcome = "transport_error"
		} else if status != http.StatusOK {
			outcome = "rejected"
		}
		var code contract.ErrorCode
		var typed contract.Error
		if json.Unmarshal(body, &typed) == nil {
			code = typed.Code
		}
		root.recordAction("create_child", parentID, outcome, status, code)
		call.done <- lifecycleHTTPResult{Status: status, Body: body, Err: err}
	}()
	return call
}

// wait blocks for a child call's terminal result and fails the test on timeout
// instead of ever returning a zero result.
func (call *lifecycleChildCall) wait(t *testing.T, timeout time.Duration) lifecycleHTTPResult {
	t.Helper()
	select {
	case result := <-call.done:
		return result
	case <-time.After(timeout):
		t.Fatalf("lifecycle child call %q did not settle within %s", call.label, timeout)
		return lifecycleHTTPResult{}
	}
}

// startLifecycleRoot wraps the shared startRoot and starts the event journal
// immediately, retaining the deliberately stalled SSE connection for explicit
// close during stopLifecycleRoot.
func (h *liveAcceptance) startLifecycleRoot(label string) *lifecycleRoot {
	process, socket, tree, stream, stalled := h.startRoot(label)
	root := &lifecycleRoot{
		process:           process,
		socket:            socket,
		tree:              tree,
		client:            unixHTTPClient(socket),
		stream:            stream,
		journal:           newLifecycleEventJournal(stream),
		stalled:           stalled,
		eventPath:         filepath.Join(h.runDir, label+"-events.sse"),
		childPIDs:         make(map[int]struct{}),
		immutableEvents:   make(map[string][]supervisor.EventEnvelope),
		externalCancelled: make(map[string]struct{}),
	}
	h.recordLifecycleDescendantPIDs(root, "root-start")
	h.captureLifecycleBoundary(root, "pre-control")
	return root
}

// stopLifecycleRoot issues a graceful root DELETE, waits for the process and
// the root event journal to close and fully drain, closes the stalled SSE
// connection, verifies the root socket is absent, and polls until every tracked
// tree session's resources are gone.
func (h *liveAcceptance) stopLifecycleRoot(root *lifecycleRoot) {
	root.mu.Lock()
	hasPostControl := root.postControl != nil
	root.mu.Unlock()
	if !hasPostControl {
		h.captureLifecycleBoundary(root, "post-control")
	}
	status, err := h.deleteLifecycleSession(root, root.tree.SessionID)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("lifecycle root DELETE = %d, %v", status, err)
	}
	h.finishStoppedLifecycleRoot(root, "root DELETE")
	h.captureLifecycleBoundary(root, "final")
	h.assertLifecycleFinalInvariants(root)
}

func (h *liveAcceptance) finishStoppedLifecycleRoot(root *lifecycleRoot, description string) {
	if err := h.waitProcess(root.process, 2*time.Minute); err != nil {
		h.t.Fatalf("lifecycle root exited after %s: %v", description, err)
	}
	if root.stalled != nil {
		_ = root.stalled.Close()
	}
	select {
	case <-root.journal.done:
	case <-time.After(30 * time.Second):
		h.t.Fatal("lifecycle root event journal did not close and drain after process exit")
	}
	var captureErr error
	select {
	case captureErr = <-root.stream.done:
		root.stream.done <- captureErr
	case <-time.After(30 * time.Second):
		h.t.Fatal("lifecycle root SSE capture did not report completion")
	}
	if err := validateLifecycleSSECompletion(captureErr); err != nil {
		h.t.Fatal(err)
	}
	if err := validateLifecycleSSECapture(root.eventPath, root.journal.snapshot()); err != nil {
		h.t.Fatalf("validate lifecycle SSE capture: %v", err)
	}
	if _, err := os.Stat(root.socket); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("lifecycle root socket remains: %v", err)
	}
	h.poll(2*time.Minute, "lifecycle root tree cleanup", func() bool {
		return h.sessionsAbsent(h.sessionIDs())
	})
}

// lifecycleExternalCancellationIDs returns the session identities to record as
// externally cancelled for a DELETE. A non-root target records exactly that
// target; a root DELETE records the root plus every currently projected
// descendant from the supplied tree, or only the root when the tree snapshot
// failed (fails closed so an unrecorded open descendant is not broadly excused).
// An empty target records nothing.
func lifecycleExternalCancellationIDs(target string, tree supervisor.NodeSnapshot, treeOK bool) []string {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	if target != tree.SessionID {
		return []string{target}
	}
	if !treeOK {
		return []string{target}
	}
	return treeSessionIDs(tree)
}

func (root *lifecycleRoot) recordExternalCancellation(ids []string) {
	root.mu.Lock()
	defer root.mu.Unlock()
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			root.externalCancelled[trimmed] = struct{}{}
		}
	}
}

func (h *liveAcceptance) deleteLifecycleSession(root *lifecycleRoot, sessionID string) (int, error) {
	// Record the exact external cancellation identity at the action boundary
	// before the request so a truncated descendant stream can be admitted only
	// for an accepted DELETE. Request/status validation still fails the scenario
	// on any rejected or transport-failed DELETE before final validation.
	var current supervisor.NodeSnapshot
	treeOK := unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &current) == nil
	root.recordExternalCancellation(lifecycleExternalCancellationIDs(sessionID, current, treeOK))

	status, _, err := unixRequest(root.client, http.MethodDelete, "/v1/sessions/"+url.PathEscape(sessionID), nil)
	outcome := "accepted"
	if err != nil {
		outcome = "transport_error"
	} else if status != http.StatusAccepted {
		outcome = "rejected"
	}
	root.recordAction("delete_session", sessionID, outcome, status, "")
	return status, err
}

// lifecycleRPCCommand routes one command through the root socket and requires
// Pi's exact successful response envelope for that same command.
func (h *liveAcceptance) lifecycleRPCCommand(root *lifecycleRoot, sessionID string, command map[string]any) map[string]any {
	commandType, _ := command["type"].(string)
	if strings.TrimSpace(commandType) == "" {
		h.t.Fatalf("lifecycle RPC command has no nonempty type: %#v", command)
	}
	response := h.rpc(root.client, sessionID, command)
	responseType, _ := response["type"].(string)
	responseCommand, _ := response["command"].(string)
	if responseType != "response" || responseCommand != commandType || response["success"] != true {
		root.recordAction(commandType, sessionID, "rejected", 0, "")
		h.t.Fatalf("lifecycle RPC %q acknowledgement for %s was not exact: %#v", commandType, sessionID, response)
	}
	root.recordAction(commandType, sessionID, "accepted", 0, "")
	return response
}

func (root *lifecycleRoot) recordAction(operation, target, outcome string, status int, code contract.ErrorCode) {
	root.mu.Lock()
	defer root.mu.Unlock()
	root.actions = append(root.actions, lifecycleAction{
		Sequence: len(root.actions) + 1, Operation: operation, TargetSessionID: target,
		Outcome: outcome, HTTPStatus: status, ErrorCode: code,
	})
}

// lifecycleGetState decodes the full typed get_state response and validates
// the response envelope instead of relying on map assertions for control gates.
func (h *liveAcceptance) lifecycleGetState(root *lifecycleRoot, sessionID string) pirpc.GetStateData {
	response := h.lifecycleRPCCommand(root, sessionID, map[string]any{"type": "get_state"})
	data, ok := response["data"].(map[string]any)
	if !ok {
		h.t.Fatalf("get_state response for %s has no object data: %#v", sessionID, response)
	}
	streaming, streamingOK := data["isStreaming"].(bool)
	pending, pendingOK := data["pendingMessageCount"].(float64)
	if !streamingOK || !pendingOK || pending < 0 || pending != float64(int(pending)) {
		h.t.Fatalf("get_state response for %s lacks exact streaming/pending state: %#v", sessionID, response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		h.t.Fatalf("encode typed get_state response for %s: %v", sessionID, err)
	}
	var state pirpc.GetStateResponse
	if err := json.Unmarshal(encoded, &state); err != nil {
		h.t.Fatalf("decode typed get_state response for %s: %v", sessionID, err)
	}
	if state.Type != "response" || state.Command != "get_state" || !state.Success ||
		state.Data.IsStreaming != streaming || state.Data.PendingMessageCount != int(pending) {
		h.t.Fatalf("typed get_state response for %s was not exact: %#v", sessionID, state)
	}
	return state.Data
}

// waitLifecycleStreaming admits a control command only after one tree snapshot
// reports the target running and typed get_state reports active streaming.
func (h *liveAcceptance) waitLifecycleStreaming(root *lifecycleRoot, sessionID, description string) pirpc.GetStateData {
	var observed pirpc.GetStateData
	h.poll(4*time.Minute, description, func() bool {
		var tree supervisor.NodeSnapshot
		if unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &tree) != nil {
			return false
		}
		node, ok := lifecycleSnapshotByID(tree, sessionID)
		if !ok || node.Lifecycle != string(supervisor.LifecycleRunning) {
			return false
		}
		state := h.lifecycleGetState(root, sessionID)
		if !state.IsStreaming {
			return false
		}
		observed = state
		return true
	})
	return observed
}

func lifecycleSnapshotByID(tree supervisor.NodeSnapshot, sessionID string) (supervisor.NodeSnapshot, bool) {
	if tree.SessionID == sessionID {
		return tree, true
	}
	for _, child := range tree.Children {
		if matched, ok := lifecycleSnapshotByID(child, sessionID); ok {
			return matched, true
		}
	}
	return supervisor.NodeSnapshot{}, false
}

// waitLifecycleSettlement is for durable roots: it first observes exactly one
// new terminal settlement, then reads typed state over the still-open root
// transport. A nonterminal state is never cached; every eligible poll re-reads
// get_state until the terminal requirements hold.
func (h *liveAcceptance) waitLifecycleSettlement(root *lifecycleRoot, sessionID string, settledBefore int, requireEmptyPending bool, description string) pirpc.GetStateData {
	var observed pirpc.GetStateData
	h.poll(4*time.Minute, description, func() bool {
		settled := root.journal.countPi(sessionID, "agent_settled", "")
		if settled > settledBefore+1 {
			h.t.Fatalf("%s emitted %d new agent_settled events, want exactly 1", description, settled-settledBefore)
		}
		if settled != settledBefore+1 {
			return false
		}
		state := h.lifecycleGetState(root, sessionID)
		if state.IsStreaming || (requireEmptyPending && state.PendingMessageCount != 0) {
			return false
		}
		observed = state
		return true
	})
	return observed
}

// waitLifecycleSettlementEvent observes one child settlement solely through
// the durable root journal. It deliberately does not probe the child's routed
// RPC API, which may disappear immediately after the terminal child result.
func (h *liveAcceptance) waitLifecycleSettlementEvent(root *lifecycleRoot, sessionID string, settledBefore int, description string) {
	h.poll(4*time.Minute, description, func() bool {
		settled := root.journal.countPi(sessionID, "agent_settled", "")
		if settled > settledBefore+1 {
			h.t.Fatalf("%s emitted %d new agent_settled events, want exactly 1", description, settled-settledBefore)
		}
		return settled == settledBefore+1
	})
}

func (h *liveAcceptance) waitLifecycleSettlementTotal(root *lifecycleRoot, sessionID string, want int, description string) {
	h.poll(4*time.Minute, description, func() bool {
		got := root.journal.countPi(sessionID, "agent_settled", "")
		if got > want {
			h.t.Fatalf("%s settlements = %d, want exactly %d", description, got, want)
		}
		return got == want
	})
}

func (h *liveAcceptance) assertLifecycleSettlementTotals(root *lifecycleRoot, expected map[string]int, description string) {
	if err := validateLifecycleSettlementTotals(root.journal.snapshot(), expected); err != nil {
		h.t.Fatalf("%s after drained event boundary: %v", description, err)
	}
}

func (h *liveAcceptance) lifecycleLastAssistantText(root *lifecycleRoot, sessionID string) string {
	response := h.rpc(root.client, sessionID, map[string]any{"type": "get_last_assistant_text"})
	text, err := validateLifecycleLastAssistantText(response)
	if err != nil {
		h.t.Fatalf("exact final assistant text response for %s: %v", sessionID, err)
	}
	root.recordAction("get_last_assistant_text", sessionID, "accepted", 0, "")
	return text
}

// assertRootUsable sends the generic short prompt containing the marker, then
// delegates to the shared prompt-accepting settlement/final-text/state helper.
// Every non-abort caller retains this generic wrapper.
func (h *liveAcceptance) assertRootUsable(root *lifecycleRoot, marker string) {
	h.assertRootUsablePrompt(root, marker, "Reply with exactly "+marker+".")
}

// lifecycleInterruptControlProbe builds the deterministic /present_e2e_question
// extension command for the supplied marker, retaining the complete marker
// exactly once. Empty markers are rejected because they cannot identify the
// exact projected question.
func lifecycleInterruptControlProbe(marker string) string {
	if strings.TrimSpace(marker) == "" {
		panic("lifecycle interrupt control probe requires a nonempty marker")
	}
	return "/present_e2e_question " + marker
}

// selectLifecycleQuestion selects exactly one pending question for the target
// session whose title equals the supplied marker and returns its nonempty ID.
// It rejects a missing target session, no exact title match, a match on the
// wrong session, an empty ID, or duplicate exact matches.
func selectLifecycleQuestion(tree supervisor.NodeSnapshot, sessionID, marker string) (string, error) {
	node, ok := lifecycleSnapshotByID(tree, sessionID)
	if !ok {
		return "", fmt.Errorf("target session %s not present in tree", sessionID)
	}
	var matches []string
	for _, question := range node.Questions {
		if question.Title == marker {
			matches = append(matches, question.ID)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no pending question with exact title %q for session %s", marker, sessionID)
	case 1:
		if strings.TrimSpace(matches[0]) == "" {
			return "", fmt.Errorf("matching question for session %s has empty id", sessionID)
		}
		return matches[0], nil
	default:
		return "", fmt.Errorf("duplicate pending questions with exact title %q for session %s", marker, sessionID)
	}
}

// assertLifecycleRootControlAfterInterrupt proves the same root Pi/supervisor
// control plane remains addressable and usable after an interrupt by driving
// the installed /present_e2e_question extension command: exact prompt
// acknowledgement, root question projection with the exact marker title,
// exactly one answer routed through the direct question-response route with
// HTTP 204, question removal, and typed idle/zero state. Extension commands are
// handled without a model generation, so no agent_settled is expected or
// manufactured.
func (h *liveAcceptance) assertLifecycleRootControlAfterInterrupt(root *lifecycleRoot, marker string) {
	rootSessionID := root.tree.SessionID
	h.lifecycleRPCCommand(root, rootSessionID, map[string]any{
		"type": "prompt", "message": lifecycleInterruptControlProbe(marker),
	})

	var questionID string
	h.poll(2*time.Minute, "controlled question after interrupt for "+rootSessionID, func() bool {
		var tree supervisor.NodeSnapshot
		if unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &tree) != nil {
			return false
		}
		id, err := selectLifecycleQuestion(tree, rootSessionID, marker)
		if err != nil {
			return false
		}
		questionID = id
		return true
	})

	status, _, err := unixRequest(root.client, http.MethodPost,
		"/v1/sessions/"+url.PathEscape(rootSessionID)+"/questions/"+url.PathEscape(questionID)+"/response",
		map[string]any{"value": "deterministic answer"})
	root.recordAction("answer_question", rootSessionID, "answered", status, "")
	if err != nil || status != http.StatusNoContent {
		h.t.Fatalf("answer controlled question %s for %s: status=%d err=%v", questionID, rootSessionID, status, err)
	}

	h.poll(2*time.Minute, "controlled question removal after answer for "+rootSessionID, func() bool {
		var tree supervisor.NodeSnapshot
		if unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &tree) != nil {
			return false
		}
		_, err := selectLifecycleQuestion(tree, rootSessionID, marker)
		return err != nil
	})

	state := h.lifecycleGetState(root, rootSessionID)
	if state.IsStreaming || state.PendingMessageCount != 0 {
		h.t.Fatalf("root %s after interrupt control probe is streaming=%t pending=%d, want idle/zero", rootSessionID, state.IsStreaming, state.PendingMessageCount)
	}
}

// assertRootUsablePrompt sends a prompt containing the marker, requires its
// exact acknowledgement and exactly one new settlement, then checks the final
// text and typed non-streaming state over the still-open transport.
func (h *liveAcceptance) assertRootUsablePrompt(root *lifecycleRoot, marker, prompt string) {
	before := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	h.lifecycleRPCCommand(root, root.tree.SessionID, map[string]any{"type": "prompt", "message": prompt})
	h.waitLifecycleSettlement(root, root.tree.SessionID, before, false, "new root settlement after marker prompt")
	text := h.lifecycleLastAssistantText(root, root.tree.SessionID)
	if !strings.Contains(text, marker) {
		h.t.Fatalf("root final text %q does not contain marker %q", text, marker)
	}
}

func (h *liveAcceptance) recordLifecycleDescendantPIDs(root *lifecycleRoot, label string) []int {
	pids := descendantPIDs(root.process.cmd.Process.Pid)
	root.mu.Lock()
	for _, pid := range pids {
		root.childPIDs[pid] = struct{}{}
	}
	root.mu.Unlock()
	h.writeJSON(label+"-owned-descendant-pids.json", map[string]any{
		"rootPid": root.process.cmd.Process.Pid, "descendantPids": pids,
	})
	return pids
}

func (h *liveAcceptance) captureLifecycleBoundary(root *lifecycleRoot, boundary string) {
	h.recordLifecycleDescendantPIDs(root, "lifecycle-"+boundary)
	var tree supervisor.NodeSnapshot
	treeErr := unixJSON(root.client, http.MethodGet, "/v1/tree", nil, &tree)
	stopped := false
	select {
	case <-root.process.done:
		stopped = true
	default:
	}
	if treeErr != nil && !stopped {
		h.t.Fatalf("capture lifecycle %s tree: %v", boundary, treeErr)
	}
	var treePointer *supervisor.NodeSnapshot
	if treeErr == nil {
		h.trackTree(tree)
		copy := tree
		treePointer = &copy
	}
	evidence := &lifecycleBoundaryEvidence{
		Tree: lifecycleTreeEvidence{
			Available: treePointer != nil, RootStopped: stopped,
			RootSocketPresent: pathExists(root.socket), Tree: treePointer,
		},
		Resources: h.snapshotResources("lifecycle-" + boundary + "-resources"),
	}
	root.mu.Lock()
	switch boundary {
	case "pre-control":
		root.preControl = evidence
	case "post-control":
		root.postControl = evidence
	case "final":
		root.final = evidence
	default:
		root.mu.Unlock()
		h.t.Fatalf("unknown lifecycle boundary %q", boundary)
		return
	}
	root.mu.Unlock()
}

func (h *liveAcceptance) assertLifecycleFinalInvariants(root *lifecycleRoot) {
	events := root.journal.snapshot()
	root.mu.Lock()
	preControl := root.preControl
	postControl := root.postControl
	final := root.final
	actions := append([]lifecycleAction(nil), root.actions...)
	ownedDescendantPIDs := make([]int, 0, len(root.childPIDs))
	for pid := range root.childPIDs {
		ownedDescendantPIDs = append(ownedDescendantPIDs, pid)
	}
	immutable := make(map[string][]supervisor.EventEnvelope, len(root.immutableEvents))
	for sessionID, frozen := range root.immutableEvents {
		immutable[sessionID] = append([]supervisor.EventEnvelope(nil), frozen...)
	}
	externallyCancelled := make(map[string]struct{}, len(root.externalCancelled))
	for sessionID := range root.externalCancelled {
		externallyCancelled[sessionID] = struct{}{}
	}
	root.mu.Unlock()
	if err := validateLifecycleFinalEvents(events, externallyCancelled); err != nil {
		h.t.Fatalf("final lifecycle event invariants: %v", err)
	}
	if preControl == nil || postControl == nil || final == nil {
		h.t.Fatalf("lifecycle boundary evidence incomplete: pre=%t post=%t final=%t", preControl != nil, postControl != nil, final != nil)
	}
	if err := validateLifecycleFinalBoundary(*final); err != nil {
		h.t.Fatalf("final lifecycle boundary: %v", err)
	}
	select {
	case <-root.process.done:
	default:
		h.t.Fatalf("owned lifecycle root process %d remains running", root.process.cmd.Process.Pid)
	}
	if pathExists(root.socket) {
		h.t.Fatalf("owned lifecycle root socket remains: %s", root.socket)
	}
	for _, sessionID := range h.sessionIDs() {
		if pathExists(h.recursiveDescendantSocketPath(sessionID)) {
			h.t.Fatalf("descendant socket remains for session %s", sessionID)
		}
	}
	for _, pid := range ownedDescendantPIDs {
		if pathExists(filepath.Join("/proc", fmt.Sprint(pid))) {
			h.t.Fatalf("observed owned lifecycle descendant process %d remains", pid)
		}
	}
	for sessionID, frozen := range immutable {
		current := lifecycleEventsForSession(events, sessionID)
		if len(current) != len(frozen) {
			h.t.Fatalf("natural child %s events changed after sibling controls: before=%d after=%d", sessionID, len(frozen), len(current))
		}
		for index := range frozen {
			if frozen[index].Seq != current[index].Seq || frozen[index].SourceSeq != current[index].SourceSeq ||
				frozen[index].Kind != current[index].Kind || string(frozen[index].Payload) != string(current[index].Payload) {
				h.t.Fatalf("natural child %s event %d changed after sibling controls", sessionID, index)
			}
		}
	}
	if !sameResourceNames(h.baseline, final.Resources) {
		h.t.Fatalf("final lifecycle resources differ from exact baseline: %s", resourceDiff(h.baseline, final.Resources))
	}
	h.writeJSON("lifecycle-events.json", events)
	h.writeJSON("lifecycle-actions.json", actions)
	h.writeJSON("lifecycle-pre-control-tree.json", preControl.Tree)
	h.writeJSON("lifecycle-post-control-tree.json", postControl.Tree)
	h.writeJSON("lifecycle-final-tree.json", final.Tree)
	h.writeJSON("lifecycle-pre-control-resources.json", preControl.Resources)
	h.writeJSON("lifecycle-post-control-resources.json", postControl.Resources)
	h.writeJSON("lifecycle-final-resources.json", final.Resources)
}

func lifecycleEventsForSession(events []supervisor.EventEnvelope, sessionID string) []supervisor.EventEnvelope {
	var selected []supervisor.EventEnvelope
	for _, event := range events {
		if event.SessionID == sessionID {
			selected = append(selected, cloneLifecycleEnvelope(event))
		}
	}
	return selected
}

func (root *lifecycleRoot) freezeLifecycleEvents(sessionID string, events []supervisor.EventEnvelope) {
	root.mu.Lock()
	root.immutableEvents[sessionID] = lifecycleEventsForSession(events, sessionID)
	root.mu.Unlock()
}

func (h *liveAcceptance) assertLifecycleOwnedProcessesStopped() {
	h.mu.Lock()
	processes := append([]*acceptanceProcess(nil), h.processes...)
	h.mu.Unlock()
	for _, process := range processes {
		select {
		case <-process.done:
		default:
			h.t.Fatalf("owned lifecycle process %d remains running", process.cmd.Process.Pid)
		}
		if pathExists(filepath.Join("/proc", fmt.Sprint(process.cmd.Process.Pid))) {
			h.t.Fatalf("owned lifecycle process %d remains present", process.cmd.Process.Pid)
		}
	}
}

// assertLifecycleProxyQuiet reads the owned proxy log after the proxy exited
// and fails if it contains a diagnostic warning, a bounded-capacity failure, or
// any configured remote credential substring. Failure output identifies only
// the matched safe class/name, never raw diagnostic arguments or credentials.
func (h *liveAcceptance) assertLifecycleProxyQuiet() {
	data, err := os.ReadFile(filepath.Join(h.runDir, "proxy.log"))
	if err != nil {
		h.t.Fatalf("read owned proxy log: %v", err)
	}
	text := string(data)
	var matched []string
	for _, marker := range []string{"proxy internal warning", "pi RPC event consumer exceeded bounded capacity"} {
		if strings.Contains(text, marker) {
			matched = append(matched, marker)
		}
	}
	for _, name := range []string{"GH_TOKEN", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_OAUTH_TOKEN", "ANTHROPIC_AUTH_TOKEN"} {
		credential := strings.TrimSpace(os.Getenv(name))
		if credential != "" && strings.Contains(text, credential) {
			matched = append(matched, "configured remote credential "+name)
		}
	}
	if len(matched) != 0 {
		h.t.Fatalf("owned proxy log leaked diagnostic or credential class: %s", strings.Join(matched, ", "))
	}
}

// runLifecycleScenario owns the lifecycle test lifecycle: authorization gate
// before any side effect, a fresh harness, local config, reviewed build, owned
// proxy, the scenario, proxy shutdown, proxy-log inspection, exact baseline,
// and success only after every assertion passes.
func runLifecycleScenario(t *testing.T, label string, run func(*liveAcceptance)) {
	requireLiveSupervisorAuthorization(t)
	if os.Getenv("KANEDIAS_E2E_EXTERNAL_PROXY") == "1" {
		t.Fatalf("lifecycle suite requires an owned proxy; KANEDIAS_E2E_EXTERNAL_PROXY=1 is not supported")
	}
	harness := newLiveAcceptance(t)
	defer harness.close()

	harness.prepareLifecycleConfig()
	harness.buildReviewedCheckout()
	harness.startProxy()
	run(harness)
	harness.stopProxy()
	harness.assertLifecycleOwnedProcessesStopped()
	harness.assertLifecycleProxyQuiet()
	harness.assertBaseline("after-" + label)
	harness.success = true
}
