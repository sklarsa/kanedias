package manager

import (
	"encoding/json"
	"regexp"

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
	Type       string `json:"type"`
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	Status     string `json:"status"`
	IsError    bool   `json:"isError"`
}

func (p *activityProjector) applyToolStart(event supervisor.EventEnvelope) {
	var payload toolPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	item := ActivityItem{
		Seq:        event.Seq,
		Kind:       "tool_execution_start",
		Label:      "Tool: " + payload.ToolName,
		ToolCallID: payload.ToolCallID,
		ToolName:   payload.ToolName,
		Status:     "running",
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
		p.items[idx].Status = "running"
	}
}

func (p *activityProjector) applyToolEnd(event supervisor.EventEnvelope) {
	var payload toolPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	if idx, ok := p.tools[payload.ToolCallID]; ok && idx < len(p.items) {
		p.items[idx].Status = "done"
		p.items[idx].IsError = payload.IsError
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
