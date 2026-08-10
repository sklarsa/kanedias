//go:build incus

package supervisor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/config"
	"github.com/sklarsa/kanedias/internal/supervisor"
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
	if err := validateLifecycleModelPolicy(cfg, "local-executor", "Qwen3.6-27B-GGUF"); err != nil {
		t.Fatalf("valid local policy: %v", err)
	}
	worker := cfg.Workers["reviewer"]
	worker.ModelType = "paid"
	cfg.Workers["reviewer"] = worker
	if err := validateLifecycleModelPolicy(cfg, "local-executor", "Qwen3.6-27B-GGUF"); err == nil || !strings.Contains(err.Error(), "reviewer") {
		t.Fatalf("mixed worker policy error = %v", err)
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
	events := []supervisor.EventEnvelope{
		lifecyclePiEnvelope(1, "root", `{"type":"tool_execution_start","toolCallId":"one","toolName":"delegate_session","args":{"workerType":"reviewer","kind":"read","context":"fresh","task":"return MARKER_ONE"}}`),
		lifecyclePiEnvelope(2, "root", `{"type":"tool_execution_start","toolCallId":"two","toolName":"delegate_session","args":{"workerType":"reviewer","kind":"read","context":"fresh","task":"return MARKER_TWO"}}`),
		lifecyclePiEnvelope(3, "root", `{"type":"tool_execution_start","toolCallId":"three","toolName":"delegate_session","args":{"workerType":"reviewer","kind":"read","context":"fresh","task":"return MARKER_THREE"}}`),
		lifecyclePiEnvelope(4, "root", `{"type":"tool_execution_end","toolCallId":"two","toolName":"delegate_session","isError":false,"result":{"content":[{"type":"text","text":"MARKER_TWO"}]}}`),
		lifecyclePiEnvelope(5, "root", `{"type":"tool_execution_end","toolCallId":"one","toolName":"delegate_session","isError":false,"result":{"content":[{"type":"text","text":"MARKER_ONE"}]}}`),
		lifecyclePiEnvelope(6, "root", `{"type":"tool_execution_end","toolCallId":"three","toolName":"delegate_session","isError":false,"result":{"content":[{"type":"text","text":"MARKER_THREE"}]}}`),
	}
	if err := validateLifecycleModelToolEvents(events, "root", markers, true); err != nil {
		t.Fatalf("valid parallel model tools: %v", err)
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
		{name: "missing result marker", edit: func(events []supervisor.EventEnvelope) []supervisor.EventEnvelope {
			events[5].Payload = json.RawMessage(strings.Replace(string(events[5].Payload), "MARKER_THREE", "missing", 1))
			return events
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cloned := make([]supervisor.EventEnvelope, len(events))
			for index, event := range events {
				cloned[index] = cloneLifecycleEnvelope(event)
			}
			if err := validateLifecycleModelToolEvents(test.edit(cloned), "root", markers, true); err == nil {
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
	return supervisor.EventEnvelope{Seq: seq, SessionID: sessionID, Kind: "pi", Payload: json.RawMessage(payload)}
}

// validateLifecycleModelPolicy resolves the whole session model policy and
// requires the root and every configured worker to resolve to one exact
// provider/model pair. Failures identify the worker by name so a mixed live
// configuration is diagnosed against the intended local model rather than a
// silent provider drift.
func validateLifecycleModelPolicy(cfg config.Config, provider, model string) error {
	policy, err := cfg.DefaultSessionModelPolicy()
	if err != nil {
		return err
	}
	if policy.Root.Provider != provider || policy.Root.Model != model {
		return fmt.Errorf("root model policy %s/%s, want %s/%s", policy.Root.Provider, policy.Root.Model, provider, model)
	}
	for _, name := range policy.WorkerNames() {
		worker := policy.Workers[name]
		if worker.Provider != provider || worker.Model != model {
			return fmt.Errorf("worker %q model policy %s/%s, want %s/%s", name, worker.Provider, worker.Model, provider, model)
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
	process *acceptanceProcess
	socket  string
	tree    supervisor.NodeSnapshot
	client  *http.Client
	stream  *sseCapture
	journal *lifecycleEventJournal
	stalled net.Conn
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

// validateLifecycleModelToolEvents accepts only the requested root
// delegate_session calls, exact reviewer/read/fresh arguments, one successful
// marked result per call, and (when requested) starts for the whole batch
// before any matching tool ends.
func validateLifecycleModelToolEvents(events []supervisor.EventEnvelope, rootID string, markers []string, requireParallel bool) error {
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

	endsByID := make(map[string][]toolEvent, len(starts))
	for _, end := range ends {
		if _, expected := startsByID[end.payload.ToolCallID]; expected {
			endsByID[end.payload.ToolCallID] = append(endsByID[end.payload.ToolCallID], end)
		}
	}
	firstEnd := len(events)
	lastStart := -1
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
		if !strings.Contains(string(end.payload.Result), markerByID[id]) {
			return fmt.Errorf("delegate_session %q result lacks marker %q", id, markerByID[id])
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
	if err := validateLifecycleModelPolicy(h.cfg, "local-executor", "Qwen3.6-27B-GGUF"); err != nil {
		h.t.Fatalf("lifecycle local model policy: %v", err)
	}
}

// startLifecycleChildCall tracks a child-create POST in h.async and returns a
// handle the caller can wait on for the terminal lifecycleHTTPResult. A
// JSON-safe artifact records status, body text, and the serialized error.
func (h *liveAcceptance) startLifecycleChildCall(client *http.Client, parentID, label, task string) *lifecycleChildCall {
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
		status, body, err := unixRequest(client, http.MethodPost,
			"/v1/sessions/"+url.PathEscape(parentID)+"/children", request)
		h.writeJSON(label+"-child-call-result.json", map[string]any{
			"status": status, "body": string(body), "error": errorString(err),
		})
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
	return &lifecycleRoot{
		process: process,
		socket:  socket,
		tree:    tree,
		client:  unixHTTPClient(socket),
		stream:  stream,
		journal: newLifecycleEventJournal(stream),
		stalled: stalled,
	}
}

// stopLifecycleRoot issues a graceful root DELETE, waits for the process,
// closes the stalled SSE connection, verifies the root socket is absent, and
// polls until every tracked tree session's resources are gone.
func (h *liveAcceptance) stopLifecycleRoot(root *lifecycleRoot) {
	status, _, err := unixRequest(root.client, http.MethodDelete, "/v1/sessions/"+root.tree.SessionID, nil)
	if err != nil || status != http.StatusAccepted {
		h.t.Fatalf("lifecycle root DELETE = %d, %v", status, err)
	}
	if err := h.waitProcess(root.process, 2*time.Minute); err != nil {
		h.t.Fatalf("lifecycle root exited after DELETE: %v", err)
	}
	if root.stalled != nil {
		_ = root.stalled.Close()
	}
	if _, err := os.Stat(root.socket); !errors.Is(err, os.ErrNotExist) {
		h.t.Fatalf("lifecycle root socket remains: %v", err)
	}
	h.poll(2*time.Minute, "lifecycle root tree cleanup", func() bool {
		return h.sessionsAbsent(treeSessionIDs(root.tree))
	})
}

// assertRootUsable sends a short prompt containing the marker, waits for a new
// root agent_settled in the journal, requires the marker in the final assistant
// text, and requires a successful non-streaming get_state.
func (h *liveAcceptance) assertRootUsable(root *lifecycleRoot, marker string) {
	before := root.journal.countPi(root.tree.SessionID, "agent_settled", "")
	h.rpc(root.client, root.tree.SessionID, map[string]any{"type": "prompt", "message": "Reply with exactly " + marker + "."})
	h.poll(4*time.Minute, "new root settlement after marker prompt", func() bool {
		return root.journal.countPi(root.tree.SessionID, "agent_settled", "") > before
	})
	text := h.lastAssistantText(root.client, root.tree.SessionID)
	if !strings.Contains(text, marker) {
		h.t.Fatalf("root final text %q does not contain marker %q", text, marker)
	}
	state := h.rpc(root.client, root.tree.SessionID, map[string]any{"type": "get_state"})
	if state["success"] != true {
		h.t.Fatalf("get_state failed on usable root: %#v", state)
	}
	data, _ := state["data"].(map[string]any)
	if streaming, _ := data["isStreaming"].(bool); streaming {
		h.t.Fatalf("get_state isStreaming still true on usable root: %#v", state)
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
	harness := newLiveAcceptance(t)
	defer harness.close()

	harness.prepareLifecycleConfig()
	harness.buildReviewedCheckout()
	harness.startProxy()
	run(harness)
	harness.stopProxy()
	harness.assertLifecycleProxyQuiet()
	harness.assertBaseline("after-" + label)
	harness.success = true
}
