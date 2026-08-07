package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
)

type NodeSnapshot struct {
	SessionID       string                `json:"sessionId"`
	PiSessionID     string                `json:"piSessionId,omitempty"`
	SessionFile     string                `json:"sessionFile,omitempty"`
	ParentSessionID string                `json:"parentSessionId,omitempty"`
	RootSessionID   string                `json:"rootSessionId"`
	Kind            contract.ChildKind    `json:"kind"`
	Context         contract.ContextMode  `json:"context"`
	WorkerType      string                `json:"workerType,omitempty"`
	Model           contract.ModelProfile `json:"model"`
	Lifecycle       string                `json:"lifecycle"`
	Questions       []QuestionSummary     `json:"pendingQuestions"`
	Children        []NodeSnapshot        `json:"children"`
}

type LocalSession struct {
	identity  Identity
	rpc       *pirpc.Client
	events    *EventBroker
	questions *QuestionStore
	lifecycle *lifecycle

	bindMu  sync.Mutex
	mu      sync.RWMutex
	binding PiBinding
	model   contract.ModelProfile
}

func NewLocalSession(identity Identity, rpc *pirpc.Client, events *EventBroker) *LocalSession {
	if events == nil {
		events = NewEventBroker()
	}
	session := &LocalSession{
		identity:  identity,
		rpc:       rpc,
		events:    events,
		questions: NewQuestionStore(rpc),
		lifecycle: newLifecycle(LifecycleStarting),
	}
	go session.drainEvents()
	return session
}

func (session *LocalSession) Bind(ctx context.Context) error {
	session.bindMu.Lock()
	defer session.bindMu.Unlock()

	session.mu.RLock()
	alreadyBound := session.binding.SessionID != "" || session.binding.SessionFile != ""
	session.mu.RUnlock()
	if alreadyBound {
		return invariantf("Pi session is already bound")
	}

	raw, err := session.rpc.Call(ctx, json.RawMessage(`{"type":"get_state"}`))
	if err != nil {
		return err
	}
	state, err := decodeGetState(raw)
	if err != nil {
		return err
	}
	if err := validatePiBinding(state.Data.SessionID, state.Data.SessionFile); err != nil {
		return err
	}

	model := modelFromGetState(state.Data)
	session.mu.Lock()
	session.binding = PiBinding{SessionID: state.Data.SessionID, SessionFile: state.Data.SessionFile}
	session.model = model
	session.mu.Unlock()

	if err := session.lifecycle.Transition(LifecycleReady); err != nil {
		return err
	}
	if state.Data.IsStreaming {
		_ = session.lifecycle.Transition(LifecycleRunning)
	}
	return nil
}

func (session *LocalSession) CallRPC(ctx context.Context, command json.RawMessage) (json.RawMessage, error) {
	var request struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(command, &request); err != nil {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "invalid Pi RPC command: "+err.Error())
	}

	response, err := session.rpc.Call(ctx, command)
	if err != nil {
		if errors.Is(err, pirpc.ErrForbiddenCommand) {
			return nil, contract.NewError(contract.ErrorForbiddenRPC, err.Error())
		}
		return nil, err
	}

	var accepted struct {
		Success bool `json:"success"`
	}
	acceptedErr := json.Unmarshal(response, &accepted)
	if request.Type == "get_state" && acceptedErr == nil && accepted.Success {
		state, err := decodeGetState(response)
		if err != nil {
			return nil, err
		}
		if err := session.verifyBinding(state.Data.SessionID, state.Data.SessionFile); err != nil {
			return nil, err
		}
		session.mu.Lock()
		session.model = modelFromGetState(state.Data)
		session.mu.Unlock()
	}

	if acceptedErr == nil && accepted.Success {
		switch request.Type {
		case "prompt", "follow_up":
			session.markRunning()
		}
	}
	return response, nil
}

func (session *LocalSession) Snapshot() NodeSnapshot {
	identity := session.identity.Snapshot()
	session.mu.RLock()
	binding := session.binding
	model := session.model
	session.mu.RUnlock()

	questions := session.questions.Summaries()
	children := make([]NodeSnapshot, 0)
	sort.Slice(children, func(i, j int) bool { return children[i].SessionID < children[j].SessionID })
	return NodeSnapshot{
		SessionID:       identity.SessionID,
		PiSessionID:     binding.SessionID,
		SessionFile:     binding.SessionFile,
		ParentSessionID: identity.ParentID,
		RootSessionID:   identity.RootID,
		Kind:            identity.Kind,
		Context:         identity.Context,
		WorkerType:      identity.Worker,
		Model:           model,
		Lifecycle:       string(session.lifecycle.State()),
		Questions:       questions,
		Children:        children,
	}
}

func (session *LocalSession) StopRPC() error {
	session.questions.Clear()
	state := session.lifecycle.State()
	if state == LifecycleStopped {
		return session.rpc.Close()
	}
	if state != LifecycleStopping {
		if err := session.lifecycle.Transition(LifecycleStopping); err != nil {
			return err
		}
	}
	closeErr := session.rpc.Close()
	if err := session.lifecycle.Transition(LifecycleStopped); err != nil {
		return errors.Join(closeErr, err)
	}
	return closeErr
}

func (session *LocalSession) drainEvents() {
	for {
		select {
		case event := <-session.rpc.Events():
			session.handleEvent(event)
		case <-session.rpc.Done():
			for {
				select {
				case event := <-session.rpc.Events():
					session.handleEvent(event)
				default:
					session.handleRPCTermination()
					return
				}
			}
		}
	}
}

func (session *LocalSession) handleEvent(event pirpc.Event) {
	session.events.PublishLocal(session.identity.sessionID, "pi", event.Raw)
	if event.Type == "extension_ui_request" {
		_, _ = session.questions.Retain(event.Raw)
	}

	switch event.Type {
	case "agent_start":
		session.markRunning()
	case "agent_settled":
		if session.lifecycle.State() != LifecycleRunning {
			return
		}
		switch session.identity.kind {
		case contract.ChildKindRoot:
			_ = session.lifecycle.Transition(LifecycleReady)
		case contract.ChildKindWrite:
			_ = session.lifecycle.Transition(LifecycleAwaitingHandoff)
		}
	}
}

func (session *LocalSession) handleRPCTermination() {
	session.questions.Clear()
	switch session.lifecycle.State() {
	case LifecycleStarting, LifecycleReady, LifecycleRunning, LifecycleAwaitingHandoff:
		_ = session.lifecycle.Transition(LifecycleFailed)
	}
}

func (session *LocalSession) markRunning() {
	switch session.lifecycle.State() {
	case LifecycleReady, LifecycleAwaitingHandoff:
		_ = session.lifecycle.Transition(LifecycleRunning)
	}
}

func (session *LocalSession) verifyBinding(sessionID, sessionFile string) error {
	if err := validatePiBinding(sessionID, sessionFile); err != nil {
		return err
	}
	session.mu.RLock()
	binding := session.binding
	session.mu.RUnlock()
	if binding.SessionID == "" {
		return invariantf("Pi session has not been bound")
	}
	if binding.SessionID != sessionID {
		return invariantf("Pi session ID changed from %q to %q", binding.SessionID, sessionID)
	}
	if binding.SessionFile != sessionFile {
		return invariantf("Pi session file changed from %q to %q", binding.SessionFile, sessionFile)
	}
	return nil
}

func validatePiBinding(sessionID, sessionFile string) error {
	if sessionID == "" {
		return invariantf("Pi session ID is required")
	}
	if sessionFile == "" {
		return invariantf("Pi session file is required")
	}
	return nil
}

func decodeGetState(raw json.RawMessage) (pirpc.GetStateResponse, error) {
	var state pirpc.GetStateResponse
	if err := json.Unmarshal(raw, &state); err != nil {
		return state, fmt.Errorf("decode get_state response: %w", err)
	}
	if state.Type != "response" || state.Command != "get_state" {
		return state, invariantf("unexpected get_state response type %q command %q", state.Type, state.Command)
	}
	if !state.Success {
		return state, fmt.Errorf("Pi get_state failed: %s", state.Error)
	}
	return state, nil
}

func modelFromGetState(data pirpc.GetStateData) contract.ModelProfile {
	var model struct {
		Provider string `json:"provider"`
		ID       string `json:"id"`
	}
	_ = json.Unmarshal(data.Model, &model)
	return contract.ModelProfile{
		Provider:      model.Provider,
		Model:         model.ID,
		ThinkingLevel: data.ThinkingLevel,
	}
}
