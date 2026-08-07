package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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

	activityMu           sync.Mutex
	lifecycleResponseSeq uint64
	lastActivitySeq      uint64
	lastActivity         string
	drainDone            chan struct{}
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
		drainDone: make(chan struct{}),
	}
	go session.drainEvents()
	return session
}

func (session *LocalSession) Bind(ctx context.Context) error {
	return session.BindExpected(ctx, nil)
}

// BindExpected admits the first persisted Pi binding. Fork children require an
// exact match with the branch prepared by their parent; roots and fresh children
// pass nil and accept any first nonempty persisted binding.
func (session *LocalSession) BindExpected(ctx context.Context, expected *PiBinding) error {
	session.bindMu.Lock()
	defer session.bindMu.Unlock()

	session.mu.RLock()
	alreadyBound := session.binding.SessionID != "" || session.binding.SessionFile != ""
	session.mu.RUnlock()
	if alreadyBound {
		return invariantf("Pi session is already bound")
	}

	raw, responseSeq, err := session.rpc.CallWithSequence(ctx, json.RawMessage(`{"type":"get_state"}`))
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
	if expected != nil {
		if state.Data.SessionID != expected.SessionID {
			return invariantf("fork Pi session ID %q does not match admitted session ID %q", state.Data.SessionID, expected.SessionID)
		}
		if state.Data.SessionFile != expected.SessionFile {
			return invariantf("fork Pi session file %q does not match admitted session file %q", state.Data.SessionFile, expected.SessionFile)
		}
	}

	model := modelFromGetState(state.Data)
	session.mu.Lock()
	session.binding = PiBinding{SessionID: state.Data.SessionID, SessionFile: state.Data.SessionFile}
	session.model = model
	session.mu.Unlock()

	session.activityMu.Lock()
	defer session.activityMu.Unlock()
	session.lifecycleResponseSeq = responseSeq
	if err := session.lifecycle.Transition(LifecycleReady); err != nil {
		return err
	}
	if session.lastActivitySeq > responseSeq {
		session.applyActivityLocked(session.lastActivity)
	} else if state.Data.IsStreaming {
		session.markRunningLocked()
	}
	return nil
}

func (session *LocalSession) CallRPC(ctx context.Context, command json.RawMessage) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(command, &object); err != nil || object == nil {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "Pi RPC command must be a JSON object")
	}
	var requestType string
	typeRaw, ok := object["type"]
	if !ok || json.Unmarshal(typeRaw, &requestType) != nil || strings.TrimSpace(requestType) == "" {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "Pi RPC command type must be a nonempty string")
	}

	response, responseSeq, err := session.rpc.CallWithSequence(ctx, command)
	if err != nil {
		if errors.Is(err, pirpc.ErrForbiddenCommand) {
			return nil, contract.NewError(contract.ErrorForbiddenRPC, err.Error())
		}
		return nil, err
	}

	var accepted struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Success bool   `json:"success"`
	}
	acceptedErr := json.Unmarshal(response, &accepted)
	responseAccepted := acceptedErr == nil && accepted.Type == "response" && accepted.Command == requestType && accepted.Success
	if requestType == "get_state" && responseAccepted {
		state, err := decodeGetState(response)
		if err != nil {
			return nil, err
		}
		if err := session.verifyBinding(state.Data.SessionID, state.Data.SessionFile); err != nil {
			session.failTerminalInvariant()
			return nil, err
		}
		session.mu.Lock()
		session.model = modelFromGetState(state.Data)
		session.mu.Unlock()
	}

	if responseAccepted {
		switch requestType {
		case "abort":
			session.questions.CancelPending()
		case "prompt", "follow_up":
			session.activityMu.Lock()
			if responseSeq > session.lifecycleResponseSeq {
				session.lifecycleResponseSeq = responseSeq
			}
			if session.lastActivitySeq <= responseSeq {
				session.markRunningLocked()
			}
			session.activityMu.Unlock()
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
	session.activityMu.Lock()
	state := session.lifecycle.State()
	if state == LifecycleStopped {
		session.activityMu.Unlock()
		return session.rpc.Close()
	}
	if state != LifecycleStopping {
		if err := session.lifecycle.Transition(LifecycleStopping); err != nil {
			session.activityMu.Unlock()
			return err
		}
	}
	session.activityMu.Unlock()
	closeErr := session.rpc.Close()
	<-session.drainDone
	session.activityMu.Lock()
	defer session.activityMu.Unlock()
	if err := session.lifecycle.Transition(LifecycleStopped); err != nil {
		return errors.Join(closeErr, err)
	}
	return closeErr
}

// DrainDone closes only after every Pi event buffered before RPC termination has
// been reconciled and published.
func (session *LocalSession) DrainDone() <-chan struct{} { return session.drainDone }

func (session *LocalSession) drainEvents() {
	defer close(session.drainDone)
	for event := range session.rpc.Events() {
		session.handleEvent(event)
	}
	session.handleRPCTermination()
}

func (session *LocalSession) handleEvent(event pirpc.Event) {
	session.events.PublishLocal(session.identity.sessionID, "pi", event.Raw)
	if event.Type == "extension_ui_request" {
		_, _ = session.questions.Retain(event.Raw)
	}

	if event.Type == "agent_settled" || event.Type == "agent_end" {
		session.questions.CancelPending()
	}

	switch event.Type {
	case "agent_start", "agent_settled":
		session.activityMu.Lock()
		if event.Seq > session.lastActivitySeq {
			session.lastActivitySeq = event.Seq
			session.lastActivity = event.Type
		}
		if event.Seq > session.lifecycleResponseSeq {
			session.applyActivityLocked(event.Type)
		}
		session.activityMu.Unlock()
	}
}

func (session *LocalSession) handleRPCTermination() {
	session.questions.Clear()
	session.activityMu.Lock()
	defer session.activityMu.Unlock()
	switch session.lifecycle.State() {
	case LifecycleStarting, LifecycleReady, LifecycleRunning, LifecycleAwaitingHandoff:
		_ = session.lifecycle.Transition(LifecycleFailed)
	}
}

func (session *LocalSession) markRunningLocked() {
	switch session.lifecycle.State() {
	case LifecycleReady, LifecycleAwaitingHandoff:
		_ = session.lifecycle.Transition(LifecycleRunning)
	}
}

func (session *LocalSession) applyActivityLocked(activity string) {
	switch activity {
	case "agent_start":
		session.markRunningLocked()
	case "agent_settled":
		state := session.lifecycle.State()
		if state == LifecycleReady && session.identity.kind != contract.ChildKindRoot {
			// A settlement may be drained before the accepted prompt response is
			// reconciled. Every child kind still ran that admitted generation.
			session.markRunningLocked()
			state = session.lifecycle.State()
		}
		if state != LifecycleRunning {
			return
		}
		switch session.identity.kind {
		case contract.ChildKindRoot:
			_ = session.lifecycle.Transition(LifecycleReady)
		case contract.ChildKindRead:
			_ = session.lifecycle.Transition(LifecycleCompleted)
		case contract.ChildKindWrite:
			_ = session.lifecycle.Transition(LifecycleAwaitingHandoff)
		}
	}
}

func (session *LocalSession) completeHandoff() {
	session.activityMu.Lock()
	defer session.activityMu.Unlock()
	switch session.lifecycle.State() {
	case LifecycleRunning, LifecycleAwaitingHandoff:
		_ = session.lifecycle.Transition(LifecycleCompleted)
	}
}

func (session *LocalSession) failTerminalInvariant() {
	session.activityMu.Lock()
	switch session.lifecycle.State() {
	case LifecycleStarting, LifecycleReady, LifecycleRunning, LifecycleAwaitingHandoff:
		_ = session.lifecycle.Transition(LifecycleFailed)
	}
	session.activityMu.Unlock()
	session.questions.Clear()
	_ = session.rpc.Close()
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
		return state, fmt.Errorf("get_state failed on Pi: %s", state.Error)
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
