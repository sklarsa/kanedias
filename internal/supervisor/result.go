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
	// Retained events are useful to general SSE consumers but cannot classify
	// this delegated generation. Capture the local source boundary immediately
	// before constructing/submitting the exact admitted prompt.
	boundary := node.broker.SourceBoundary(identity.SessionID)
	if err := submitPrompt(ctx, local, task); err != nil {
		return contract.ReadChildResult{}, err
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
					return contract.ReadChildResult{}, childFailure(contract.ErrorChildFailed, "read event stream ended before settlement", nil)
				}
				event = liveEvent
			case <-local.rpc.Done():
				return contract.ReadChildResult{}, childFailure(contract.ErrorChildFailed, "Pi RPC stream ended before read settlement", local.rpc.Err())
			case <-ctx.Done():
				return contract.ReadChildResult{}, ctx.Err()
			}
		}
		if event.SessionID != identity.SessionID || event.Kind != "pi" || event.SourceSeq <= boundary {
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
			return contract.ReadChildResult{}, childFailure(contract.ErrorChildFailed, "decode Pi completion event", err)
		}
		switch envelope.Type {
		case "extension_error":
			return contract.ReadChildResult{}, childFailure(contract.ErrorChildFailed, "terminal extension error", fmt.Errorf("%v", envelope.Error))
		case "message_end":
			if envelope.Message != nil && envelope.Message.Role == "assistant" {
				lastStopReason = envelope.Message.StopReason
			}
		case "agent_settled":
			switch lastStopReason {
			case "stop":
			case "aborted":
				return contract.ReadChildResult{}, childFailure(contract.ErrorChildAborted, "read child was aborted", nil)
			case "error", "length":
				return contract.ReadChildResult{}, childFailure(contract.ErrorChildFailed, "read child ended with stop reason "+lastStopReason, nil)
			default:
				return contract.ReadChildResult{}, childFailure(contract.ErrorChildFailed, "read child settled without a successful final assistant message", nil)
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
		return "", childFailure(contract.ErrorChildFailed, "get final assistant text", err)
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
		return "", childFailure(contract.ErrorChildFailed, "get final assistant text", errors.New(decoded.Error))
	}
	if decoded.Data.Text == nil {
		return "", childFailure(contract.ErrorChildFailed, "get final assistant text", errors.New("null final assistant text returned by Pi"))
	}
	return *decoded.Data.Text, nil
}

func childFailure(code contract.ErrorCode, message string, cause error) error {
	return errors.Join(contract.NewError(code, message), cause)
}

// submitPrompt marshals task as a Pi prompt command, sends it over the child's
// RPC transport, and validates that Pi accepted the prompt.
func submitPrompt(ctx context.Context, local *LocalSession, task string) error {
	prompt, err := json.Marshal(map[string]string{"type": "prompt", "message": task})
	if err != nil {
		return err
	}
	response, err := local.CallRPC(ctx, prompt)
	if err != nil {
		return childFailure(contract.ErrorChildFailed, "submit child task", err)
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
		return childFailure(contract.ErrorChildFailed, "submit child task", errors.New(accepted.Error))
	}
	return nil
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
	return submitPrompt(ctx, local, task)
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
	if len(request.Repositories) > 64 {
		return contract.NewError(contract.ErrorInvalidRequest, "handoff supports at most 64 repositories")
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
		if len(repository.Repository) > 200 || len(repository.Branch) > 255 || len(repository.BaseCommit) > 64 || len(repository.HeadCommit) > 64 {
			return contract.NewError(contract.ErrorInvalidRequest, "handoff repository fields exceed safe rendering bounds")
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

func (node *Node) Handoff(ctx context.Context, request WriteHandoffRequest) (HandoffAcceptance, error) {
	identity := node.identity.Snapshot()
	if identity.Kind != contract.ChildKindWrite {
		return HandoffAcceptance{}, contract.NewError(contract.ErrorConflict, "handoff is accepted only by the owning write session")
	}
	if err := request.validate(); err != nil {
		return HandoffAcceptance{}, err
	}
	if node.deps.HandoffVerifier == nil {
		return HandoffAcceptance{}, contract.NewError(contract.ErrorInternal, "host GitHub handoff verifier is unavailable")
	}
	if err := node.deps.HandoffVerifier.Verify(ctx, request.Repositories); err != nil {
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
	return HandoffAcceptance{Accepted: true, SessionID: identity.SessionID}, nil
}

// AcknowledgeHandoff transfers acknowledgement completion from the HTTP layer.
// The fallback teardown watchdog must not start until the accepted response has
// been written and flushed to the guest connection.
func (node *Node) AcknowledgeHandoff() error {
	node.handoffMu.Lock()
	accepted := node.handoffComplete
	node.handoffMu.Unlock()
	if !accepted {
		return contract.NewError(contract.ErrorConflict, "writer handoff has not been accepted")
	}
	if node.done != nil {
		node.armHandoffShutdownWatchdog()
	}
	return nil
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
