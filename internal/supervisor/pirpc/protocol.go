package pirpc

import "encoding/json"

type commandEnvelope struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
}

type GetStateCommand struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
}

type GetStateResponse struct {
	ID      string       `json:"id,omitempty"`
	Type    string       `json:"type"`
	Command string       `json:"command"`
	Success bool         `json:"success"`
	Data    GetStateData `json:"data"`
	Error   string       `json:"error,omitempty"`
}

type GetStateData struct {
	Model                 json.RawMessage `json:"model,omitempty"`
	ThinkingLevel         string          `json:"thinkingLevel"`
	IsStreaming           bool            `json:"isStreaming"`
	IsCompacting          bool            `json:"isCompacting"`
	SteeringMode          string          `json:"steeringMode"`
	FollowUpMode          string          `json:"followUpMode"`
	SessionFile           string          `json:"sessionFile,omitempty"`
	SessionID             string          `json:"sessionId"`
	SessionName           string          `json:"sessionName,omitempty"`
	AutoCompactionEnabled bool            `json:"autoCompactionEnabled"`
	MessageCount          int             `json:"messageCount"`
	PendingMessageCount   int             `json:"pendingMessageCount"`
}

type ExtensionUIResponse struct {
	Type      string  `json:"type"`
	ID        string  `json:"id"`
	Value     *string `json:"value,omitempty"`
	Confirmed *bool   `json:"confirmed,omitempty"`
	Cancelled bool    `json:"cancelled,omitempty"`
}

var forbiddenCommandTypes = map[string]struct{}{
	"new_session":    {},
	"switch_session": {},
	"fork":           {},
	"clone":          {},
}
