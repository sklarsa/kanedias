package manager

import (
	"encoding/json"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

// allowlistedPiEvents is the set of Pi event types that receive specific
// structured projections. Unknown types produce a generic ActivityItem.
var allowlistedPiEvents = map[string]struct{}{
	"message_update":        {},
	"message_end":           {},
	"tool_execution_start":  {},
	"tool_execution_update": {},
	"tool_execution_end":    {},
	"queue_update":          {},
	"agent_start":           {},
	"agent_settled":         {},
	"extension_error":       {},
}

// activityProjector accumulates a sequence of events into a list of
// ActivityItems, tracking tool state by toolCallId.
type activityProjector struct {
	items    []ActivityItem
	tools    map[string]int // toolCallId -> index into items
	textSeq  uint64
	textBuf  string
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

func (p *activityProjector) applyPiType(event supervisor.EventEnvelope, piType string) {
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
	case "queue_update":
		p.items = append(p.items, ActivityItem{
			Seq: event.Seq, Kind: "queue_update", Label: "Queue update",
		})
	case "agent_start":
		p.items = append(p.items, ActivityItem{
			Seq: event.Seq, Kind: "agent_start", Label: "Agent started",
		})
	case "agent_settled":
		p.items = append(p.items, ActivityItem{
			Seq: event.Seq, Kind: "agent_settled", Label: "Agent settled",
		})
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
	Type    string `json:"type"`
	Message struct {
		Type    string `json:"type"`
		Content []struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"content"`
	} `json:"message"`
}

func (p *activityProjector) applyMessageUpdate(event supervisor.EventEnvelope) {
	var payload messageUpdatePayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return
	}
	for _, content := range payload.Message.Content {
		if content.Type == "tool_use" {
			continue
		}
		if content.Delta.Type == "text_delta" && content.Delta.Text != "" {
			if !p.textOpen {
				p.textOpen = true
				p.textSeq = event.Seq
				p.items = append(p.items, ActivityItem{
					Seq: event.Seq, Kind: "message_update", Label: "Message",
				})
			} else {
				// Update seq of the current open message.
				for i := len(p.items) - 1; i >= 0; i-- {
					if p.items[i].Kind == "message_update" && p.items[i].Seq == p.textSeq {
						p.items[i].Text += content.Delta.Text
						break
					}
				}
				return
			}
			if len(p.items) > 0 {
				p.items[len(p.items)-1].Text += content.Delta.Text
			}
		}
	}
}

func (p *activityProjector) applyMessageEnd(event supervisor.EventEnvelope) {
	p.textOpen = false
	p.textSeq = 0
	_ = event // message_end signals completion; no additional data extracted
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
