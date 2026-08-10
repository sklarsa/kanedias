package manager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sklarsa/kanedias/internal/attachments"
	"github.com/sklarsa/kanedias/internal/supervisor"
)

// piResponse is the typed envelope returned by every Pi RPC call.
type piResponse[T any] struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Success bool   `json:"success"`
	Data    T      `json:"data"`
	Error   string `json:"error,omitempty"`
}

// decodePiResponse unmarshals a Pi RPC response and validates type, command,
// and success fields.
func decodePiResponse[T any](raw json.RawMessage, expectedCommand string) (T, error) {
	var zero T
	if !json.Valid(raw) {
		return zero, fmt.Errorf("pi response is not valid JSON")
	}
	// Reject trailing data: json.Unmarshal uses the whole body.
	var resp piResponse[T]
	if err := json.Unmarshal(raw, &resp); err != nil {
		return zero, fmt.Errorf("decode Pi response: %w", err)
	}
	if resp.Type == "" {
		return zero, fmt.Errorf("pi response missing type field")
	}
	if resp.Command != "" && expectedCommand != "" && resp.Command != expectedCommand {
		return zero, fmt.Errorf("pi response command %q does not match expected %q", resp.Command, expectedCommand)
	}
	if !resp.Success {
		msg := resp.Error
		if msg == "" {
			msg = "pi returned success:false"
		}
		return zero, fmt.Errorf("pi command failed: %s", msg)
	}
	return resp.Data, nil
}

// mustJSON marshals v to JSON, panicking on error. Used for trusted structures.
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("mustJSON: " + err.Error())
	}
	return b
}

// actionableClient returns the rootClient for an actionable route, holding the
// manager lock only for lookup and releasing it before any network I/O.
func (m *Manager) actionableClient(sessionID string) (rootClient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quiesced || m.closed {
		return nil, errors.New("manager: quiesced, actions are disabled")
	}
	rootID, ok := m.routes[sessionID]
	if !ok {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}
	for _, h := range m.roots {
		if h.rootID == rootID {
			if !h.actionable || h.stale {
				return nil, fmt.Errorf("session %q root is not actionable", sessionID)
			}
			return h.client, nil
		}
	}
	return nil, fmt.Errorf("root %q not found for session %q", rootID, sessionID)
}

// actionableClientAndNode returns the rootClient and node for a session.
func (m *Manager) actionableClientAndNode(sessionID string) (rootClient, supervisor.NodeSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.quiesced || m.closed {
		return nil, supervisor.NodeSnapshot{}, errors.New("manager: quiesced, actions are disabled")
	}
	rootID, ok := m.routes[sessionID]
	if !ok {
		return nil, supervisor.NodeSnapshot{}, fmt.Errorf("session %q not found", sessionID)
	}
	for _, h := range m.roots {
		if h.rootID == rootID {
			if !h.actionable || h.stale {
				return nil, supervisor.NodeSnapshot{}, fmt.Errorf("session %q root is not actionable", sessionID)
			}
			node, found := findNode(h.tree, sessionID)
			if !found {
				return nil, supervisor.NodeSnapshot{}, fmt.Errorf("session %q not found in tree", sessionID)
			}
			return h.client, node, nil
		}
	}
	return nil, supervisor.NodeSnapshot{}, fmt.Errorf("root %q not found for session %q", rootID, sessionID)
}

// piStateData is the data portion of a get_state response.
type piStateData struct {
	IsStreaming bool   `json:"isStreaming"`
	SessionID   string `json:"sessionId,omitempty"`
	Model       *struct {
		Input []string `json:"input"`
	} `json:"model,omitempty"`
}

var ErrImageInputUnsupported = errors.New("manager: selected model does not support image input")

type piMessageImage struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MIMEType string `json:"mimeType"`
}

type piMessageCommand struct {
	Type    string           `json:"type"`
	Message string           `json:"message"`
	Images  []piMessageImage `json:"images,omitempty"`
}

// Steer sends a streaming steer or idle prompt to a session without images.
func (m *Manager) Steer(ctx context.Context, sessionID string, message string) error {
	return m.SendMessage(ctx, sessionID, message, nil)
}

// SendMessage sends a streaming steer or idle prompt, including native Pi
// image content when the selected model supports image input.
func (m *Manager) SendMessage(ctx context.Context, sessionID, message string, images []attachments.Image) error {
	if err := attachments.Validate(images); err != nil {
		return err
	}

	client, err := m.actionableClient(sessionID)
	if err != nil {
		return err
	}
	rawState, err := client.CallRPC(ctx, sessionID, mustJSON(map[string]any{"type": "get_state"}))
	if err != nil {
		return fmt.Errorf("get_state: %w", err)
	}
	state, err := decodePiResponse[piStateData](rawState, "get_state")
	if err != nil {
		return fmt.Errorf("get_state: %w", err)
	}

	if len(images) > 0 && !supportsImageInput(state.Model) {
		return ErrImageInputUnsupported
	}

	commandType := "prompt"
	if state.IsStreaming {
		commandType = "steer"
	}
	command := piMessageCommand{Type: commandType, Message: message}
	for _, image := range images {
		command.Images = append(command.Images, piMessageImage{
			Type:     "image",
			Data:     base64.StdEncoding.EncodeToString(image.Data),
			MIMEType: image.MIMEType,
		})
	}

	rawResp, err := client.CallRPC(ctx, sessionID, mustJSON(command))
	if err != nil {
		return err
	}
	_, err = decodePiResponse[any](rawResp, commandType)
	return err
}

func supportsImageInput(model *struct {
	Input []string `json:"input"`
}) bool {
	if model == nil {
		return false
	}
	for _, input := range model.Input {
		if input == "image" {
			return true
		}
	}
	return false
}

// Interrupt aborts the current turn of a session.
func (m *Manager) Interrupt(ctx context.Context, sessionID string) error {
	client, err := m.actionableClient(sessionID)
	if err != nil {
		return err
	}
	raw, err := client.CallRPC(ctx, sessionID, mustJSON(map[string]any{"type": "abort"}))
	if err != nil {
		return err
	}
	_, err = decodePiResponse[any](raw, "abort")
	return err
}

// StopSession stops one session subtree through its owning root. If the owning
// root is not actionable (stale or otherwise unreachable — e.g. it crashed and
// left an orphaned socket), Stop cannot reach it, so the root is evicted from
// the fleet immediately rather than leaving its card lingering until the next
// discovery pass notices the socket is gone (#11). Stop on a live root keeps the
// graceful RPC stop.
func (m *Manager) StopSession(ctx context.Context, sessionID string) error {
	m.mu.Lock()
	if m.quiesced || m.closed {
		m.mu.Unlock()
		return errors.New("manager: quiesced, actions are disabled")
	}
	rootID, ok := m.routes[sessionID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session %q not found", sessionID)
	}
	var target *rootHandle
	var socketPath string
	for sp, h := range m.roots {
		if h.rootID == rootID {
			target, socketPath = h, sp
			break
		}
	}
	if target == nil {
		m.mu.Unlock()
		return fmt.Errorf("root %q not found for session %q", rootID, sessionID)
	}
	if !target.actionable || target.stale {
		// Evict immediately: Stop can't reach a stale/unreachable root, so drop
		// it from the fleet now instead of waiting for discovery to notice.
		removed := m.removeRootLocked(socketPath)
		m.mu.Unlock()
		if removed != nil {
			m.drainAndCloseDisplaced(removed)
			m.bumpFleetRevision()
		}
		return nil
	}
	client := target.client
	m.mu.Unlock()
	return client.Stop(ctx, sessionID)
}

// AnswerQuestion forwards a raw answer to one pending question.
func (m *Manager) AnswerQuestion(ctx context.Context, sessionID string, questionID string, answer json.RawMessage) error {
	client, err := m.actionableClient(sessionID)
	if err != nil {
		return err
	}
	return client.AnswerQuestion(ctx, sessionID, questionID, answer)
}

// piSessionStats is the data portion of a get_session_stats response.
type piSessionStats struct {
	SessionID         string          `json:"sessionId,omitempty"`
	UserMessages      int             `json:"userMessages"`
	AssistantMessages int             `json:"assistantMessages"`
	ToolCalls         int             `json:"toolCalls"`
	ToolResults       int             `json:"toolResults"`
	TotalMessages     int             `json:"totalMessages"`
	InputTokens       int64           `json:"inputTokens"`
	OutputTokens      int64           `json:"outputTokens"`
	CacheReadTokens   int64           `json:"cacheReadTokens"`
	CacheWriteTokens  int64           `json:"cacheWriteTokens"`
	TotalTokens       int64           `json:"totalTokens"`
	Cost              float64         `json:"cost"`
	ContextUsage      *piContextUsage `json:"contextUsage,omitempty"`
}

// piContextUsage reflects Pi 0.83.0's nullable context state.
type piContextUsage struct {
	Tokens        *float64 `json:"tokens"`
	ContextWindow float64  `json:"contextWindow"`
	Percent       *float64 `json:"percent"`
}

func projectStats(data piSessionStats) SessionStats {
	stats := SessionStats{
		UserMessages:      data.UserMessages,
		AssistantMessages: data.AssistantMessages,
		ToolCalls:         data.ToolCalls,
		ToolResults:       data.ToolResults,
		TotalMessages:     data.TotalMessages,
		Tokens: TokenStats{
			Input:      data.InputTokens,
			Output:     data.OutputTokens,
			CacheRead:  data.CacheReadTokens,
			CacheWrite: data.CacheWriteTokens,
			Total:      data.TotalTokens,
		},
		Cost: data.Cost,
	}
	if data.ContextUsage != nil {
		stats.ContextUsage = &ContextUsage{
			Tokens:        data.ContextUsage.Tokens,
			ContextWindow: data.ContextUsage.ContextWindow,
			Percent:       data.ContextUsage.Percent,
		}
	}
	return stats
}

// SessionStats returns typed Pi metrics for one actionable session.
func (m *Manager) SessionStats(ctx context.Context, sessionID string) (SessionStats, error) {
	client, node, err := m.actionableClientAndNode(sessionID)
	if err != nil {
		return SessionStats{}, err
	}
	raw, err := client.CallRPC(ctx, sessionID, mustJSON(map[string]any{"type": "get_session_stats"}))
	if err != nil {
		return SessionStats{}, err
	}
	data, err := decodePiResponse[piSessionStats](raw, "get_session_stats")
	if err != nil {
		return SessionStats{}, err
	}
	if data.SessionID != "" && node.PiSessionID != "" && data.SessionID != node.PiSessionID {
		return SessionStats{}, fmt.Errorf("pi stats identity mismatch: got %q, expected %q", data.SessionID, node.PiSessionID)
	}
	return projectStats(data), nil
}
