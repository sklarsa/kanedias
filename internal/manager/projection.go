package manager

import (
	"bytes"
	"encoding/json"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// questionIDPattern constrains question IDs to a safe charset before they are
// exposed to the web view. A supervisor is untrusted: its question IDs arrive
// raw from /v1/tree and are interpolated into browser contexts (URLs, DOM
// attributes). Restricting the charset here — at the data boundary — is the
// load-bearing defense against an ID like `'); fetch(...)//` breaking out of a
// string literal that the browser later re-decodes and evaluates.
//
// The charset deliberately excludes quotes, slashes, angle brackets, and
// whitespace so an ID can never terminate an attribute, a URL segment, or a JS
// string literal. IDs longer than 128 bytes are also rejected.
var questionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// ValidQuestionID reports whether id is a safe question identifier: 1..128
// characters drawn only from [A-Za-z0-9_.:-]. IDs that fail this check must be
// treated as non-answerable and must never be interpolated into a browser
// context.
func ValidQuestionID(id string) bool {
	return questionIDPattern.MatchString(id)
}

// activityProjector accumulates a sequence of events into a list of
// ActivityItems, tracking tool state by toolCallId.
type activityProjector struct {
	items    []ActivityItem
	tools    map[string]int // toolCallId -> index into items
	textSeq  uint64
	textOpen bool
}

func newActivityProjector() *activityProjector {
	return &activityProjector{tools: make(map[string]int)}
}

// Apply processes one event and updates the projector state.
func (p *activityProjector) Apply(event supervisor.EventEnvelope) {
	if event.Kind != "pi" {
		return
	}
	var wrapper struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(event.Payload, &wrapper); err != nil {
		return
	}
	p.applyPiType(event, wrapper.Type)
}

// ignoredPiTypes are pure lifecycle/flow events that carry no transcript content
// (agent/turn/message framing, retries, UI requests). They would otherwise show
// up as noisy "Pi event: …" lines and drown out the real messages and tool calls.
var ignoredPiTypes = map[string]bool{
	"agent_start":          true,
	"agent_settled":        true,
	"agent_end":            true,
	"queue_update":         true,
	"turn_start":           true,
	"turn_end":             true,
	"message_start":        true,
	"auto_retry_start":     true,
	"auto_retry_end":       true,
	"extension_ui_request": true,
}

func (p *activityProjector) applyPiType(event supervisor.EventEnvelope, piType string) {
	if ignoredPiTypes[piType] {
		return
	}
	switch piType {
	case "message_update":
		p.applyMessageUpdate(event)
	case "message_end":
		p.applyMessageEnd(event)
	case "tool_execution_start":
		p.applyToolStart(event)
	case "tool_execution_update":
		p.applyToolUpdate(event)
	case "tool_execution_end":
		p.applyToolEnd(event)
	case "extension_error":
		var payload struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(event.Payload, &payload)
		p.items = append(p.items, ActivityItem{
			Seq: event.Seq, Kind: "extension_error", Label: "Extension error",
			Text: payload.Message, IsError: true,
		})
	default:
		// Unknown Pi event: generic item without raw payload.
		p.items = append(p.items, ActivityItem{
			Seq: event.Seq, Kind: "event", Label: "Pi event: " + piType,
		})
	}
}

type messageUpdatePayload struct {
	AssistantMessageEvent struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
}

func (p *activityProjector) applyMessageUpdate(event supervisor.EventEnvelope) {
	var payload messageUpdatePayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	if payload.AssistantMessageEvent.Type != "text_delta" || payload.AssistantMessageEvent.Delta == "" {
		return
	}
	if !p.textOpen {
		p.textOpen = true
		p.textSeq = event.Seq
		p.items = append(p.items, ActivityItem{
			Seq: event.Seq, Kind: "message_update", Label: "Message",
		})
	}
	for i := len(p.items) - 1; i >= 0; i-- {
		if p.items[i].Kind == "message_update" && p.items[i].Seq == p.textSeq {
			p.items[i].Text += payload.AssistantMessageEvent.Delta
			return
		}
	}
}

type messageEndPayload struct {
	Message struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
		Content      []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

func (p *activityProjector) applyMessageEnd(event supervisor.EventEnvelope) {
	p.textOpen = false
	p.textSeq = 0

	var payload messageEndPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	if payload.Message.Role == "user" {
		var text string
		for _, content := range payload.Message.Content {
			if content.Type == "text" {
				text += content.Text
			}
		}
		if text != "" {
			p.items = append(p.items, ActivityItem{
				Seq: event.Seq, Kind: "user_message", Label: "You", Text: text,
			})
		}
		return
	}
	if payload.Message.Role != "assistant" || payload.Message.StopReason != "error" || payload.Message.ErrorMessage == "" {
		return
	}
	if len(p.items) > 0 {
		last := p.items[len(p.items)-1]
		if last.IsError && last.Text == payload.Message.ErrorMessage {
			return
		}
	}
	p.items = append(p.items, ActivityItem{
		Seq: event.Seq, Kind: "model_error", Label: "Model error",
		Text: payload.Message.ErrorMessage, IsError: true,
	})
}

type toolPayload struct {
	Type          string          `json:"type"`
	ToolCallID    string          `json:"toolCallId"`
	ToolName      string          `json:"toolName"`
	Status        string          `json:"status"`
	IsError       bool            `json:"isError"`
	Args          json.RawMessage `json:"args"`
	PartialResult json.RawMessage `json:"partialResult"`
	Result        json.RawMessage `json:"result"`
}

// truncationMarker is the visible suffix appended to any tool display field
// that exceeds maxToolDisplayBytes. Its byte length is reserved inside
// boundedDisplay so the total projected field never exceeds the cap.
const truncationMarker = "\n… display truncated …"

// boundedDisplay caps a display string at maxToolDisplayBytes, reserving room
// for truncationMarker and backtracking to the nearest valid UTF-8 boundary so
// the projected text is always decodable. Empty input stays empty/unitruncated.
func boundedDisplay(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if len(s) <= maxToolDisplayBytes {
		return s, false
	}
	cut := maxToolDisplayBytes - len(truncationMarker)
	if cut < 0 {
		cut = 0
	}
	prefix := s[:cut]
	for len(prefix) > 0 && !utf8.ValidString(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	return prefix + truncationMarker, true
}

// formatToolJSON pretty-prints a raw JSON argument block with two-space
// indentation and bounds the result. Malformed JSON falls back to the bounded
// original string so no raw data is ever widened beyond the cap.
func formatToolJSON(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return boundedDisplay(string(raw))
	}
	return boundedDisplay(buf.String())
}

// toolContentBlock is the allowlisted shape read out of a tool's
// result/partialResult content array. Only typed text and image-mime blocks are
// projected; everything else is ignored.
type toolContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	MimeType string `json:"mimeType"`
}

// formatToolResult projects a tool result/partialResult into a bounded display
// string. Text content blocks are joined in order and image blocks are
// summarized as "[image: <mime>]"; when no supported content block is present
// the indented result JSON is used instead. All output is bounded at
// maxToolDisplayBytes.
func formatToolResult(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var payload struct {
		Content []toolContentBlock `json:"content"`
	}
	if err := json.Unmarshal(raw, &payload); err == nil {
		var sb strings.Builder
		for _, block := range payload.Content {
			switch {
			case block.Type == "text" && block.Text != "":
				sb.WriteString(block.Text)
			case block.Type != "text" && block.MimeType != "":
				sb.WriteString("[image: " + block.MimeType + "]")
			}
		}
		if sb.Len() > 0 {
			return boundedDisplay(sb.String())
		}
	}
	return formatToolJSON(raw)
}

// capRunes truncates s to at most n display characters (runes), preserving
// valid UTF-8. It is used to bound the bash summary line.
func capRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// summarizeTool builds the one-line human summary shown in the collapsed tool
// card. It decodes only the argument keys it needs for presentation and never
// projects raw argument data wholesale.
func summarizeTool(toolName string, args json.RawMessage) string {
	if toolName == "" {
		return ""
	}
	var m map[string]json.RawMessage
	if len(args) > 0 {
		_ = json.Unmarshal(args, &m)
	}
	firstString := func(keys ...string) string {
		for _, k := range keys {
			raw, ok := m[k]
			if !ok {
				continue
			}
			var s string
			if json.Unmarshal(raw, &s) == nil && s != "" {
				return s
			}
		}
		return ""
	}
	switch toolName {
	case "bash":
		cmd := firstString("command")
		if cmd == "" {
			if raw, ok := m["commands"]; ok {
				var list []string
				if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
					cmd = list[0]
				}
			}
		}
		if i := strings.IndexByte(cmd, '\n'); i >= 0 {
			cmd = cmd[:i]
		}
		if cmd == "" {
			return toolName
		}
		return capRunes("$ "+cmd, 160)
	case "read", "write", "edit", "ls":
		p := firstString("path", "file_path")
		if p == "" {
			return toolName
		}
		return toolName + " " + p
	case "grep", "find":
		pat := firstString("pattern")
		p := firstString("path", "file_path")
		if p == "" {
			p = "."
		}
		if pat == "" {
			return toolName + " in " + p
		}
		return toolName + " " + pat + " in " + p
	default:
		return toolName
	}
}

// toolLanguageByExt maps common source-path extensions to the Highlight.js
// language alias used to highlight a tool's output block.
var toolLanguageByExt = map[string]string{
	"go": "go", "js": "js", "ts": "ts", "tsx": "tsx", "jsx": "jsx",
	"py": "py", "rb": "rb", "rs": "rs", "java": "java", "c": "c",
	"cpp": "cpp", "cs": "cs", "php": "php", "sh": "sh", "sql": "sql",
	"html": "html", "css": "css", "scss": "scss", "json": "json",
	"yaml": "yaml", "xml": "xml", "md": "md", "dockerfile": "dockerfile",
}

// toolLanguage infers the output-highlight language from the source path a
// read/write/edit tool is operating on. Tools with no source-path inference
// return "" so the browser auto-detects the output language.
func toolLanguage(toolName string, args json.RawMessage) string {
	switch toolName {
	case "read", "write", "edit":
	default:
		return ""
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(args, &m)
	raw, _ := m["path"]
	var p string
	_ = json.Unmarshal(raw, &p)
	if p == "" {
		raw, _ = m["file_path"]
		_ = json.Unmarshal(raw, &p)
	}
	if p == "" {
		return ""
	}
	ext := strings.ToLower(path.Ext(p))
	ext = strings.TrimPrefix(ext, ".")
	return toolLanguageByExt[ext]
}

func (p *activityProjector) applyToolStart(event supervisor.EventEnvelope) {
	var payload toolPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	argsDisplay, argsTrunc := formatToolJSON(payload.Args)
	item := ActivityItem{
		Seq:           event.Seq,
		Kind:          "tool_execution_start",
		Label:         "Tool: " + payload.ToolName,
		ToolCallID:    payload.ToolCallID,
		ToolName:      payload.ToolName,
		Status:        "running",
		IsTool:        true,
		ToolSummary:   summarizeTool(payload.ToolName, payload.Args),
		ToolArgs:      argsDisplay,
		ToolLanguage:  toolLanguage(payload.ToolName, payload.Args),
		ToolTruncated: argsTrunc,
	}
	p.tools[payload.ToolCallID] = len(p.items)
	p.items = append(p.items, item)
}

func (p *activityProjector) applyToolUpdate(event supervisor.EventEnvelope) {
	var payload toolPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	if idx, ok := p.tools[payload.ToolCallID]; ok && idx < len(p.items) {
		out, trunc := formatToolResult(payload.PartialResult)
		p.items[idx].Status = "running"
		p.items[idx].ToolOutput = out
		p.items[idx].ToolTruncated = p.items[idx].ToolTruncated || trunc
	}
}

func (p *activityProjector) applyToolEnd(event supervisor.EventEnvelope) {
	var payload toolPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	if idx, ok := p.tools[payload.ToolCallID]; ok && idx < len(p.items) {
		out, trunc := formatToolResult(payload.Result)
		p.items[idx].Status = "done"
		p.items[idx].IsError = payload.IsError
		p.items[idx].ToolOutput = out
		p.items[idx].ToolTruncated = p.items[idx].ToolTruncated || trunc
	}
}

// Items returns the projected activity items.
func (p *activityProjector) Items() []ActivityItem {
	return append([]ActivityItem(nil), p.items...)
}

// projectActivity projects Pi events from events that belong to sessionID.
func projectActivity(events []supervisor.EventEnvelope, sessionID string) []ActivityItem {
	projector := newActivityProjector()
	for _, event := range events {
		if event.SessionID == sessionID {
			projector.Apply(event)
		}
	}
	return projector.Items()
}
