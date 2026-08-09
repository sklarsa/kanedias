package manager

import (
	"encoding/json"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

func piEvent(seq uint64, session, piType string, extra map[string]any) supervisor.EventEnvelope {
	payload := map[string]any{"type": piType}
	for k, v := range extra {
		payload[k] = v
	}
	raw, _ := json.Marshal(payload)
	return supervisor.EventEnvelope{
		Seq: seq, SessionID: session, SourceSeq: seq, Kind: "pi",
		Payload: json.RawMessage(raw),
	}
}

func TestProjectActivityFiltersAgentLifecycleNoise(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "agent_start", nil),
		piEvent(2, "s", "agent_settled", nil),
		piEvent(3, "s", "queue_update", nil),
		piEvent(4, "s", "turn_start", nil),
		piEvent(5, "s", "message_start", nil),
		piEvent(6, "s", "turn_end", nil),
		piEvent(7, "s", "agent_end", nil),
		piEvent(8, "s", "auto_retry_start", nil),
		piEvent(9, "s", "extension_ui_request", nil),
	}
	items := projectActivity(events, "s")
	if len(items) != 0 {
		t.Fatalf("agent lifecycle noise should not be surfaced, got %d items: %#v", len(items), items)
	}
}

func TestProjectActivitySurfacesOnlyContentInTurn(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "turn_start", nil),
		piEvent(2, "s", "message_start", nil),
		piEvent(3, "s", "message_update", map[string]any{
			"message": map[string]any{"content": []any{
				map[string]any{"delta": map[string]any{"type": "text_delta", "text": "hello"}},
			}},
		}),
		piEvent(4, "s", "message_end", nil),
		piEvent(5, "s", "turn_end", nil),
		piEvent(6, "s", "agent_end", nil),
	}
	items := projectActivity(events, "s")
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 content item, got %d: %#v", len(items), items)
	}
	if items[0].Kind != "message_update" || items[0].Text != "hello" {
		t.Fatalf("content item = %#v", items[0])
	}
}

func TestProjectActivityUnknownEventBecomesGeneric(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "future_event", map[string]any{"secret": "data"}),
	}
	items := projectActivity(events, "s")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if items[0].Kind != "event" {
		t.Fatalf("kind = %q, want event", items[0].Kind)
	}
	if items[0].Label != "Pi event: future_event" {
		t.Fatalf("label = %q", items[0].Label)
	}
	// Must not contain raw payload text.
	if items[0].Text != "" {
		t.Fatalf("text should be empty for unknown event, got %q", items[0].Text)
	}
}

func TestProjectActivityToolLifecycle(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "tool_execution_start", map[string]any{
			"toolCallId": "tc-1", "toolName": "bash",
		}),
		piEvent(2, "s", "tool_execution_update", map[string]any{
			"toolCallId": "tc-1",
		}),
		piEvent(3, "s", "tool_execution_end", map[string]any{
			"toolCallId": "tc-1", "isError": false,
		}),
	}
	items := projectActivity(events, "s")
	if len(items) != 1 {
		t.Fatalf("expected 1 tool item, got %d: %#v", len(items), items)
	}
	if items[0].ToolCallID != "tc-1" || items[0].ToolName != "bash" {
		t.Fatalf("tool item = %#v", items[0])
	}
	if items[0].Status != "done" {
		t.Fatalf("status = %q, want done", items[0].Status)
	}
}

func TestProjectActivityFiltersOtherSessions(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s1", "tool_execution_start", map[string]any{"toolCallId": "a", "toolName": "bash"}),
		piEvent(2, "s2", "tool_execution_start", map[string]any{"toolCallId": "b", "toolName": "bash"}),
		piEvent(3, "s1", "tool_execution_end", map[string]any{"toolCallId": "a"}),
	}
	items := projectActivity(events, "s1")
	if len(items) != 1 {
		t.Fatalf("expected 1 item for s1, got %d", len(items))
	}
	items2 := projectActivity(events, "s2")
	if len(items2) != 1 {
		t.Fatalf("expected 1 item for s2, got %d", len(items2))
	}
}

func TestProjectActivityExtensionError(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "extension_error", map[string]any{"message": "something failed"}),
	}
	items := projectActivity(events, "s")
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1", len(items))
	}
	if !items[0].IsError {
		t.Fatal("extension_error should be IsError=true")
	}
}

func TestProjectActivityNonPiEventsIgnored(t *testing.T) {
	events := []supervisor.EventEnvelope{
		{Seq: 1, SessionID: "s", Kind: "system", Payload: json.RawMessage(`{"type":"ready"}`)},
	}
	items := projectActivity(events, "s")
	if len(items) != 0 {
		t.Fatalf("expected 0 items for non-pi event, got %d", len(items))
	}
}
