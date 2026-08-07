package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

var ErrResultAlreadyCompleted = errors.New("result cell is already completed")

func (node *Node) RunReadTask(ctx context.Context, task string) (contract.ReadChildResult, error) {
	identity := node.identity.Snapshot()
	if identity.Kind != contract.ChildKindRead {
		return contract.ReadChildResult{}, contract.NewError(contract.ErrorConflict, "only read children can produce read results")
	}
	if strings.TrimSpace(task) == "" {
		return contract.ReadChildResult{}, contract.NewError(contract.ErrorInvalidRequest, "read task is required")
	}
	node.mu.RLock()
	local := node.local
	node.mu.RUnlock()
	if local == nil {
		return contract.ReadChildResult{}, contract.NewError(contract.ErrorChildUnavailable, "child Pi RPC is not ready")
	}

	subscription := node.broker.Subscribe()
	defer subscription.Close()
	prompt, err := json.Marshal(map[string]string{"type": "prompt", "message": task})
	if err != nil {
		return contract.ReadChildResult{}, err
	}
	response, err := local.CallRPC(ctx, prompt)
	if err != nil {
		return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildFailed, "submit child task", err)
	}
	var accepted struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(response, &accepted); err != nil || accepted.Type != "response" || accepted.Command != "prompt" || !accepted.Success {
		if accepted.Error == "" {
			accepted.Error = "Pi rejected the prompt"
		}
		return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildFailed, "submit child task", errors.New(accepted.Error))
	}

	pending := append([]EventEnvelope(nil), subscription.Replay...)
	var lastStopReason string
	for {
		var event EventEnvelope
		if len(pending) > 0 {
			event, pending = pending[0], pending[1:]
		} else {
			select {
			case liveEvent, ok := <-subscription.Events:
				if !ok {
					return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildFailed, "read event stream ended before settlement", nil)
				}
				event = liveEvent
			case <-local.rpc.Done():
				return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildFailed, "Pi RPC stream ended before read settlement", local.rpc.Err())
			case <-ctx.Done():
				return contract.ReadChildResult{}, ctx.Err()
			}
		}
		if event.SessionID != identity.SessionID || event.Kind != "pi" {
			continue
		}
		var envelope struct {
			Type    string `json:"type"`
			Message *struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
			} `json:"message,omitempty"`
			Error any `json:"error,omitempty"`
		}
		if err := json.Unmarshal(event.Payload, &envelope); err != nil {
			return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildFailed, "decode Pi completion event", err)
		}
		switch envelope.Type {
		case "extension_error":
			return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildFailed, "terminal extension error", fmt.Errorf("%v", envelope.Error))
		case "message_end":
			if envelope.Message != nil && envelope.Message.Role == "assistant" {
				lastStopReason = envelope.Message.StopReason
			}
		case "agent_settled":
			switch lastStopReason {
			case "stop":
			case "aborted":
				return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildAborted, "read child was aborted", nil)
			case "error", "length":
				return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildFailed, "read child ended with stop reason "+lastStopReason, nil)
			default:
				return contract.ReadChildResult{}, childReadFailure(contract.ErrorChildFailed, "read child settled without a successful final assistant message", nil)
			}
			output, err := lastAssistantText(ctx, local)
			if err != nil {
				return contract.ReadChildResult{}, err
			}
			return contract.ReadChildResult{Kind: contract.ChildKindRead, WorkerType: identity.Worker, SessionID: identity.SessionID, Output: output}, nil
		}
	}
}

func lastAssistantText(ctx context.Context, local *LocalSession) (string, error) {
	response, err := local.CallRPC(ctx, json.RawMessage(`{"type":"get_last_assistant_text"}`))
	if err != nil {
		return "", childReadFailure(contract.ErrorChildFailed, "get final assistant text", err)
	}
	var decoded struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Success bool   `json:"success"`
		Data    struct {
			Text *string `json:"text"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(response, &decoded); err != nil || decoded.Type != "response" || decoded.Command != "get_last_assistant_text" || !decoded.Success {
		return "", childReadFailure(contract.ErrorChildFailed, "get final assistant text", errors.New(decoded.Error))
	}
	if decoded.Data.Text == nil {
		return "", childReadFailure(contract.ErrorChildFailed, "get final assistant text", errors.New("Pi returned null final assistant text"))
	}
	return *decoded.Data.Text, nil
}

func childReadFailure(code contract.ErrorCode, message string, cause error) error {
	return errors.Join(contract.NewError(code, message), cause)
}

func (node *Node) RunWriteTask(ctx context.Context, task string) error {
	identity := node.identity.Snapshot()
	if identity.Kind != contract.ChildKindWrite {
		return contract.NewError(contract.ErrorConflict, "only write children can run write tasks")
	}
	if strings.TrimSpace(task) == "" {
		return contract.NewError(contract.ErrorInvalidRequest, "write task is required")
	}
	node.mu.RLock()
	local := node.local
	node.mu.RUnlock()
	if local == nil {
		return contract.NewError(contract.ErrorChildUnavailable, "child Pi RPC is not ready")
	}
	prompt, err := json.Marshal(map[string]string{"type": "prompt", "message": task})
	if err != nil {
		return err
	}
	response, err := local.CallRPC(ctx, prompt)
	if err != nil {
		return childReadFailure(contract.ErrorChildFailed, "submit child task", err)
	}
	var accepted struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(response, &accepted); err != nil || accepted.Type != "response" || accepted.Command != "prompt" || !accepted.Success {
		if accepted.Error == "" {
			accepted.Error = "Pi rejected the prompt"
		}
		return childReadFailure(contract.ErrorChildFailed, "submit child task", errors.New(accepted.Error))
	}
	return nil
}

type WriteHandoffRequest struct {
	Repositories []contract.RepositoryHandoff `json:"repositories"`
	Summary      string                       `json:"summary"`
	Verification []string                     `json:"verification"`
}

type HandoffAcceptance struct {
	Accepted  bool   `json:"accepted"`
	SessionID string `json:"sessionId"`
}

func (request WriteHandoffRequest) validate() error {
	if len(request.Repositories) == 0 {
		return contract.NewError(contract.ErrorInvalidRequest, "handoff requires at least one repository")
	}
	if strings.TrimSpace(request.Summary) == "" {
		return contract.NewError(contract.ErrorInvalidRequest, "handoff summary is required")
	}
	seen := make(map[string]struct{}, len(request.Repositories))
	for _, repository := range request.Repositories {
		for field, value := range map[string]string{
			"repository": repository.Repository, "baseCommit": repository.BaseCommit,
			"branch": repository.Branch, "headCommit": repository.HeadCommit,
		} {
			if strings.TrimSpace(value) == "" {
				return contract.NewError(contract.ErrorInvalidRequest, "handoff repository "+field+" is required")
			}
		}
		if _, duplicate := seen[repository.Repository]; duplicate {
			return contract.NewError(contract.ErrorInvalidRequest, "handoff repositories must be unique")
		}
		seen[repository.Repository] = struct{}{}
	}
	for _, evidence := range request.Verification {
		if strings.TrimSpace(evidence) == "" {
			return contract.NewError(contract.ErrorInvalidRequest, "handoff verification entries must be nonempty")
		}
	}
	return nil
}

func (node *Node) Handoff(_ context.Context, request WriteHandoffRequest) (HandoffAcceptance, error) {
	identity := node.identity.Snapshot()
	if identity.Kind != contract.ChildKindWrite {
		return HandoffAcceptance{}, contract.NewError(contract.ErrorConflict, "handoff is accepted only by the owning write session")
	}
	if err := request.validate(); err != nil {
		return HandoffAcceptance{}, err
	}

	node.handoffMu.Lock()
	defer node.handoffMu.Unlock()
	if node.handoffComplete {
		return HandoffAcceptance{}, contract.NewError(contract.ErrorConflict, "writer handoff is already complete")
	}
	if node.reportWrite == nil {
		return HandoffAcceptance{}, contract.NewError(contract.ErrorConflict, "writer handoff reporter is unavailable")
	}
	result := contract.WriteChildResult{
		Kind: contract.ChildKindWrite, WorkerType: identity.Worker, SessionID: identity.SessionID,
		Repositories: append([]contract.RepositoryHandoff(nil), request.Repositories...),
		Summary:      request.Summary, Verification: append([]string(nil), request.Verification...),
	}
	// Record before forwarding. If the synchronous report write is rejected,
	// roll the tentative record back so the writer remains live and retryable.
	node.handoffResult = &result
	if err := node.reportWrite(result); err != nil {
		node.handoffResult = nil
		return HandoffAcceptance{}, contract.NewError(contract.ErrorChildUnavailable, "forward writer handoff to parent: "+err.Error())
	}
	node.handoffComplete = true
	node.mu.RLock()
	local := node.local
	node.mu.RUnlock()
	if local != nil {
		local.completeHandoff()
	}
	if node.done != nil {
		node.armHandoffShutdownWatchdog()
	}
	return HandoffAcceptance{Accepted: true, SessionID: identity.SessionID}, nil
}

func (node *Node) handoffAccepted() bool {
	node.handoffMu.Lock()
	defer node.handoffMu.Unlock()
	return node.handoffComplete
}

type TerminalResult struct {
	Read  *contract.ReadChildResult
	Write *contract.WriteChildResult
}

func (result TerminalResult) validate(completionErr error) error {
	if completionErr != nil {
		if result.Read != nil || result.Write != nil {
			return invariantf("failed completion must not carry a terminal result")
		}
		return nil
	}
	if (result.Read == nil) == (result.Write == nil) {
		return invariantf("successful completion must carry exactly one terminal result")
	}
	if result.Read != nil {
		if err := result.Read.Validate(); err != nil {
			return invariantf("invalid read terminal result: %v", err)
		}
		return nil
	}
	if err := result.Write.Validate(); err != nil {
		return invariantf("invalid write terminal result: %v", err)
	}
	return nil
}

type ResultCell struct {
	mu        sync.Mutex
	done      chan struct{}
	completed bool
	result    TerminalResult
	err       error
}

func NewResultCell() *ResultCell {
	return &ResultCell{done: make(chan struct{})}
}

func (cell *ResultCell) Complete(result TerminalResult, completionErr error) error {
	cell.mu.Lock()
	if cell.completed {
		cell.mu.Unlock()
		return ErrResultAlreadyCompleted
	}
	cell.mu.Unlock()

	if err := result.validate(completionErr); err != nil {
		return err
	}
	result = cloneTerminalResult(result)

	cell.mu.Lock()
	defer cell.mu.Unlock()
	if cell.completed {
		return ErrResultAlreadyCompleted
	}
	cell.completed = true
	cell.result = result
	cell.err = completionErr
	close(cell.done)
	return nil
}

func (cell *ResultCell) Wait(ctx context.Context) (TerminalResult, error) {
	select {
	case <-cell.done:
		return cell.snapshot()
	default:
	}
	select {
	case <-cell.done:
		return cell.snapshot()
	case <-ctx.Done():
		return TerminalResult{}, ctx.Err()
	}
}

func (cell *ResultCell) Done() <-chan struct{} {
	return cell.done
}

func (cell *ResultCell) snapshot() (TerminalResult, error) {
	cell.mu.Lock()
	defer cell.mu.Unlock()
	return cloneTerminalResult(cell.result), cell.err
}

func cloneTerminalResult(result TerminalResult) TerminalResult {
	if result.Read != nil {
		read := *result.Read
		result.Read = &read
	}
	if result.Write != nil {
		write := *result.Write
		write.Repositories = append([]contract.RepositoryHandoff(nil), write.Repositories...)
		write.Verification = append([]string(nil), write.Verification...)
		result.Write = &write
	}
	return result
}
