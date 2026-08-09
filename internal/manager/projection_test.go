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

func TestProjectActivityMarksAssistantCompleteWithoutChangingIdentity(t *testing.T) {
	projector := newActivityProjector()
	projector.Apply(piEvent(7, "s", "message_update", map[string]any{
		"assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "hello"},
	}))

	streaming := projector.Items()
	if len(streaming) != 1 || streaming[0].Seq != 7 || streaming[0].Complete {
		t.Fatalf("streaming item = %#v", streaming)
	}

	projector.Apply(piEvent(8, "s", "message_end", map[string]any{
		"message": map[string]any{
			"role": "assistant", "stopReason": "stop",
			"content": []any{map[string]any{"type": "text", "text": "hello"}},
		},
	}))
	completed := projector.Items()
	if len(completed) != 1 || completed[0].Seq != 7 || !completed[0].Complete {
		t.Fatalf("completed item = %#v", completed)
	}
}

func TestProjectActivityUnmatchedMessageEndKeepsAssistantOpen(t *testing.T) {
	tests := []struct {
		name     string
		endEvent supervisor.EventEnvelope
		wantUser bool
	}{
		{
			name: "invalid JSON",
			endEvent: supervisor.EventEnvelope{
				Seq: 2, SessionID: "s", SourceSeq: 2, Kind: "pi",
				Payload: json.RawMessage(`{"type":"message_end",`),
			},
		},
		{name: "missing message", endEvent: piEvent(2, "s", "message_end", nil)},
		{name: "null message", endEvent: piEvent(2, "s", "message_end", map[string]any{"message": nil})},
		{
			name: "user message",
			endEvent: piEvent(2, "s", "message_end", map[string]any{
				"message": map[string]any{
					"role":    "user",
					"content": []any{map[string]any{"type": "text", "text": "prompt"}},
				},
			}),
			wantUser: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projector := newActivityProjector()
			projector.Apply(piEvent(1, "s", "message_update", map[string]any{
				"assistantMessageEvent": map[string]any{"type": "text_delta", "delta": "hello"},
			}))
			projector.Apply(tt.endEvent)

			items := projector.Items()
			assertOpenAssistantItem(t, items, "hello")
			var hasUser bool
			for _, item := range items {
				hasUser = hasUser || item.Kind == "user_message"
			}
			if hasUser != tt.wantUser {
				t.Fatalf("user message projected = %v, want %v: %#v", hasUser, tt.wantUser, items)
			}

			projector.Apply(piEvent(3, "s", "message_update", map[string]any{
				"assistantMessageEvent": map[string]any{"type": "text_delta", "delta": " world"},
			}))
			assertOpenAssistantItem(t, projector.Items(), "hello world")
		})
	}
}

func assertOpenAssistantItem(t *testing.T, items []ActivityItem, wantText string) {
	t.Helper()
	var assistantItems []ActivityItem
	for _, item := range items {
		if item.Kind == "message_update" {
			assistantItems = append(assistantItems, item)
		}
	}
	if len(assistantItems) != 1 {
		t.Fatalf("assistant items = %#v, want one item", assistantItems)
	}
	if assistantItems[0].Seq != 1 || assistantItems[0].Text != wantText || assistantItems[0].Complete {
		t.Fatalf("open assistant item = %#v, want seq 1 text %q and incomplete", assistantItems[0], wantText)
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
	if !items[0].Complete || !items[1].Complete {
		t.Fatalf("one-shot user/error items should be complete: %#v", items)
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
	if !items[0].Complete {
		t.Fatal("one-shot generic item should be complete")
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
	if !items[0].Complete {
		t.Fatal("completed tool should be immutable")
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
	if !items[0].Complete {
		t.Fatal("one-shot extension_error item should be complete")
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

func TestProjectActivityToolStateStages(t *testing.T) {
	p := newActivityProjector()
	p.Apply(piEvent(1, "s", "tool_execution_start", map[string]any{
		"toolCallId": "tc-1", "toolName": "read",
		"args": map[string]any{"path": "a.txt"},
	}))
	items := p.Items()
	if len(items) != 1 {
		t.Fatalf("items after start = %#v", items)
	}
	if items[0].Status != "running" {
		t.Fatalf("status after start = %q, want running", items[0].Status)
	}
	if items[0].ToolArgs == "" || items[0].ToolSummary != "read a.txt" {
		t.Fatalf("start projection = %#v", items[0])
	}

	p.Apply(piEvent(2, "s", "tool_execution_update", map[string]any{
		"toolCallId": "tc-1", "partialResult": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "partial"},
		}},
	}))
	items = p.Items()
	if items[0].ToolOutput != "partial" {
		t.Fatalf("output after update = %q, want partial", items[0].ToolOutput)
	}

	p.Apply(piEvent(3, "s", "tool_execution_end", map[string]any{
		"toolCallId": "tc-1", "isError": false,
		"result": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "final\n"},
		}},
	}))
	items = p.Items()
	if items[0].Status != "done" {
		t.Fatalf("status after end = %q, want done", items[0].Status)
	}
	// Final result must replace, not append to, the partial output.
	if items[0].ToolOutput != "final\n" {
		t.Fatalf("output after end = %q, want final (replaces partial)", items[0].ToolOutput)
	}
	if items[0].IsError {
		t.Fatal("end with isError=false must not set IsError")
	}
}

func TestProjectActivityToolIndependentlyBoundsArgsAndOutput(t *testing.T) {
	big := strings.Repeat("A", maxToolDisplayBytes+100)
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "tool_execution_start", map[string]any{
			"toolCallId": "tc-t", "toolName": "bash",
			"args": map[string]any{"command": big},
		}),
		piEvent(2, "s", "tool_execution_end", map[string]any{
			"toolCallId": "tc-t",
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
	if len(item.ToolArgs) > maxToolDisplayBytes || len(item.ToolOutput) > maxToolDisplayBytes {
		t.Fatalf("args/output exceed bound: args=%d output=%d", len(item.ToolArgs), len(item.ToolOutput))
	}
	if !item.ToolArgsTruncated {
		t.Fatal("oversized args should be flagged ToolArgsTruncated")
	}
	if !item.ToolOutputTruncated {
		t.Fatal("oversized output should be flagged ToolOutputTruncated")
	}
	if !item.ToolTruncated {
		t.Fatal("aggregate ToolTruncated should be set")
	}
	if !utf8.ValidString(item.ToolArgs) || !utf8.ValidString(item.ToolOutput) {
		t.Fatal("args/output not valid UTF-8")
	}
}

func TestProjectActivityMalformedEventPayloadSafe(t *testing.T) {
	events := []supervisor.EventEnvelope{
		{Seq: 1, SessionID: "s", Kind: "pi", Payload: json.RawMessage(`not-json-at-all`)},
		{Seq: 2, SessionID: "s", Kind: "pi", Payload: json.RawMessage(`{"type":`)},
	}
	items := projectActivity(events, "s")
	if len(items) != 0 {
		t.Fatalf("malformed payloads produced items: %#v", items)
	}
}

func TestFormatToolJSONMalformedFallsBackBounded(t *testing.T) {
	out, trunc := formatToolJSON(json.RawMessage(`{"path":`))
	if out == "" {
		t.Fatal("malformed JSON should fall back to bounded original text")
	}
	if len(out) > maxToolDisplayBytes || !utf8.ValidString(out) {
		t.Fatalf("malformed fallback unbounded/invalid: %d bytes", len(out))
	}
	_ = trunc
}

func TestProjectActivityToolMediaRedaction(t *testing.T) {
	cases := []struct {
		name   string
		result map[string]any
		banned []string
		want   string
	}{
		{
			name: "image with data and mime",
			result: map[string]any{"content": []any{
				map[string]any{"type": "image", "data": "AAAAbase64secret", "mimeType": "image/png"},
			}},
			banned: []string{"AAAAbase64secret", "\"data\""},
			want:   "[image: image/png]",
		},
		{
			name: "image missing mime",
			result: map[string]any{"content": []any{
				map[string]any{"type": "image", "data": "IMG"},
			}},
			banned: []string{"IMG"},
			want:   "[image]",
		},
		{
			name: "binary without mime",
			result: map[string]any{"content": []any{
				map[string]any{"type": "application/octet-stream", "data": "SECRETBYTES"},
			}},
			banned: []string{"SECRETBYTES", "\"data\""},
			want:   "[binary data]",
		},
		{
			name: "unrecognized binary with mime",
			result: map[string]any{"content": []any{
				map[string]any{"type": "blob", "mimeType": "application/pdf", "data": "PDFSECRET"},
			}},
			banned: []string{"PDFSECRET"},
			want:   "[binary: application/pdf]",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := []supervisor.EventEnvelope{
				piEvent(1, "s", "tool_execution_start", map[string]any{
					"toolCallId": "tc-i", "toolName": "read", "args": map[string]any{"path": "a.txt"},
				}),
				piEvent(2, "s", "tool_execution_end", map[string]any{
					"toolCallId": "tc-i", "result": c.result,
				}),
			}
			items := projectActivity(events, "s")
			if len(items) != 1 {
				t.Fatalf("items = %#v", items)
			}
			item := items[0]
			if item.ToolOutput != c.want {
				t.Fatalf("output = %q, want %q", item.ToolOutput, c.want)
			}
			combined := item.ToolArgs + item.ToolOutput + item.ToolSummary
			for _, b := range c.banned {
				if strings.Contains(combined, b) {
					t.Fatalf("media data %q leaked into projection: %#v", b, item)
				}
			}
		})
	}
}

func TestProjectActivityToolErrorStatusPreserved(t *testing.T) {
	events := []supervisor.EventEnvelope{
		piEvent(1, "s", "tool_execution_start", map[string]any{
			"toolCallId": "tc-e", "toolName": "bash", "args": map[string]any{"command": "false"},
		}),
		piEvent(2, "s", "tool_execution_end", map[string]any{
			"toolCallId": "tc-e", "isError": true,
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "err"}}},
		}),
	}
	items := projectActivity(events, "s")
	if len(items) != 1 {
		t.Fatalf("items = %#v", items)
	}
	if !items[0].IsError || items[0].Status != "done" {
		t.Fatalf("error tool state = %#v", items[0])
	}
}

func TestProjectActivityToolSummariesBoundOversizedInput(t *testing.T) {
	big := strings.Repeat("x", 1000)
	cases := []struct {
		name       string
		tool       string
		args       map[string]any
		wantSuffix string
	}{
		{"oversized path", "read", map[string]any{"path": big}, "read"},
		{"oversized pattern", "grep", map[string]any{"pattern": big, "path": "src"}, "grep"},
		{"oversized custom name", strings.Repeat("n", 1000), nil, strings.Repeat("n", 160)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			extra := map[string]any{"toolCallId": "tc-s", "toolName": c.tool}
			if c.args != nil {
				extra["args"] = c.args
			}
			items := projectActivity([]supervisor.EventEnvelope{piEvent(1, "s", "tool_execution_start", extra)}, "s")
			if len(items) != 1 {
				t.Fatalf("items = %#v", items)
			}
			sum := items[0].ToolSummary
			if n := utf8.RuneCountInString(sum); n > maxToolSummaryRunes {
				t.Fatalf("summary %d runes exceeds %d: %q", n, maxToolSummaryRunes, sum)
			}
			if strings.Contains(sum, big) {
				t.Fatalf("summary leaked full oversized input: %q", sum)
			}
			// The tool-derived label must also be bounded.
			if c.tool == strings.Repeat("n", 1000) {
				if strings.Contains(items[0].Label, strings.Repeat("n", 1000)) {
					t.Fatalf("label leaked unbounded tool name: %q", items[0].Label)
				}
				if utf8.RuneCountInString(items[0].Label) > maxToolSummaryRunes+len("Tool: ") {
					t.Fatalf("label unbounded: %q", items[0].Label)
				}
			}
			_ = c.wantSuffix
		})
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
