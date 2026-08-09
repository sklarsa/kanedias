package manager

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

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
			"assistantMessageEvent": map[string]any{
				"type": "text_delta", "contentIndex": 0, "delta": "hello",
			},
			"message": map[string]any{
				"role":       "assistant",
				"content":    []any{map[string]any{"type": "text", "text": "hello"}},
				"stopReason": "pending",
			},
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

func TestProjectActivityShowsPromptAndCoalescesRepeatedProviderError(t *testing.T) {
	errorMessage := map[string]any{
		"message": map[string]any{
			"role": "assistant", "content": []any{}, "stopReason": "error",
			"errorMessage": "Stream ended without finish_reason",
		},
	}
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "message_end", map[string]any{
			"message": map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": "Hello"}},
			},
		}),
		piEvent(2, "s", "message_end", errorMessage),
		piEvent(3, "s", "message_end", errorMessage),
	}

	items := projectActivity(events, "s")
	if len(items) != 2 {
		t.Fatalf("expected prompt and one error, got %d: %#v", len(items), items)
	}
	if items[0].Label != "You" || items[0].Text != "Hello" {
		t.Fatalf("prompt item = %#v", items[0])
	}
	if !items[1].IsError || items[1].Label != "Model error" || items[1].Text != "Stream ended without finish_reason" {
		t.Fatalf("error item = %#v", items[1])
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

func TestProjectActivityRetainsBoundedToolDisplay(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "tool_execution_start", map[string]any{
			"toolCallId": "tc-1", "toolName": "read",
			"args": map[string]any{"path": "internal/server/view.go"},
		}),
		piEvent(2, "s", "tool_execution_update", map[string]any{
			"toolCallId": "tc-1", "toolName": "read",
			"partialResult": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "package server"},
			}},
		}),
		piEvent(3, "s", "tool_execution_end", map[string]any{
			"toolCallId": "tc-1", "toolName": "read", "isError": false,
			"result": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "package server\n"},
			}},
		}),
	}
	items := projectActivity(events, "s")
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	item := items[0]
	if !item.IsTool {
		t.Fatal("tool item should be flagged IsTool")
	}
	if item.ToolSummary != "read internal/server/view.go" || item.ToolLanguage != "go" {
		t.Fatalf("tool display = %#v", item)
	}
	if !strings.Contains(item.ToolArgs, `"path": "internal/server/view.go"`) || item.ToolOutput != "package server\n" {
		t.Fatalf("tool details = %#v", item)
	}
	if item.Status != "done" || item.IsError {
		t.Fatalf("tool state = %#v", item)
	}
}

func TestToolDisplayTruncatesUTF8Safely(t *testing.T) {
	big := strings.Repeat("界", maxToolDisplayBytes)
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "tool_execution_start", map[string]any{
			"toolCallId": "tc-t", "toolName": "read",
			"args": map[string]any{"path": "a.txt"},
		}),
		piEvent(2, "s", "tool_execution_end", map[string]any{
			"toolCallId": "tc-t", "toolName": "read", "isError": false,
			"result": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": big},
			}},
		}),
	}
	items := projectActivity(events, "s")
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	item := items[0]
	if !utf8.ValidString(item.ToolOutput) {
		t.Fatal("tool output is not valid UTF-8")
	}
	if !item.ToolTruncated {
		t.Fatal("tool output should be flagged truncated")
	}
	if len(item.ToolOutput) > maxToolDisplayBytes {
		t.Fatalf("tool output exceeds %d bytes: %d", maxToolDisplayBytes, len(item.ToolOutput))
	}
	if !strings.HasSuffix(item.ToolOutput, "\n… display truncated …") {
		t.Fatalf("missing truncation marker in output tail: %q", item.ToolOutput[len(item.ToolOutput)-40:])
	}
}

func TestProjectActivityToolSummaries(t *testing.T) {
	cases := []struct {
		name     string
		toolName string
		args     map[string]any
		want     string
	}{
		{"bash", "bash", map[string]any{"command": "echo hi\ncd src"}, "$ echo hi"},
		{"grep", "grep", map[string]any{"pattern": "TODO", "path": "src"}, "grep TODO in src"},
		{"write prefers path then file_path", "write", map[string]any{"path": "a.go", "file_path": "b.rs"}, "write a.go"},
		{"custom fallback", "my_custom_tool", nil, "my_custom_tool"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			extra := map[string]any{"toolCallId": "tc-x", "toolName": c.toolName}
			if c.args != nil {
				extra["args"] = c.args
			}
			items := projectActivity([]supervisor.EventEnvelope{piEvent(1, "s", "tool_execution_start", extra)}, "s")
			if len(items) != 1 {
				t.Fatalf("items = %#v", items)
			}
			if items[0].ToolSummary != c.want {
				t.Fatalf("summary = %q, want %q", items[0].ToolSummary, c.want)
			}
		})
	}
}
