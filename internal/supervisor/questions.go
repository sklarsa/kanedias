package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
)

type QuestionSummary struct {
	ID          string   `json:"id"`
	Method      string   `json:"method"`
	Title       string   `json:"title"`
	Options     []string `json:"options,omitempty"`
	Message     string   `json:"message,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Prefill     string   `json:"prefill,omitempty"`
	Timeout     int      `json:"timeout,omitempty"`
}

type questionSender interface {
	Send(context.Context, json.RawMessage) error
}

type pendingQuestion struct {
	summary   QuestionSummary
	raw       json.RawMessage
	answering bool
}

type QuestionStore struct {
	mu         sync.Mutex
	sender     questionSender
	pending    map[string]*pendingQuestion
	terminated bool
}

type questionRequest struct {
	Type        string   `json:"type"`
	ID          string   `json:"id"`
	Method      string   `json:"method"`
	Title       string   `json:"title"`
	Options     []string `json:"options"`
	Message     string   `json:"message"`
	Placeholder string   `json:"placeholder"`
	Prefill     string   `json:"prefill"`
	Timeout     int      `json:"timeout"`
}

type questionAnswer struct {
	Value     *string `json:"value"`
	Confirmed *bool   `json:"confirmed"`
	Cancelled bool    `json:"cancelled"`
}

func NewQuestionStore(sender questionSender) *QuestionStore {
	return &QuestionStore{sender: sender, pending: make(map[string]*pendingQuestion)}
}

func (store *QuestionStore) Retain(raw json.RawMessage) (bool, error) {
	var request questionRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return false, fmt.Errorf("decode extension UI request: %w", err)
	}
	if request.Type != "extension_ui_request" {
		return false, fmt.Errorf("decode extension UI request: unexpected type %q", request.Type)
	}
	if !isDialogMethod(request.Method) {
		return false, nil
	}
	if strings.TrimSpace(request.ID) == "" {
		return false, invariantf("extension UI dialog ID is required")
	}
	if strings.TrimSpace(request.Title) == "" {
		return false, invariantf("extension UI dialog title is required")
	}
	if request.Method == "select" && len(request.Options) == 0 {
		return false, invariantf("extension UI select options are required")
	}

	question := &pendingQuestion{
		summary: QuestionSummary{
			ID:          request.ID,
			Method:      request.Method,
			Title:       request.Title,
			Options:     append([]string(nil), request.Options...),
			Message:     request.Message,
			Placeholder: request.Placeholder,
			Prefill:     request.Prefill,
			Timeout:     request.Timeout,
		},
		raw: cloneRaw(raw),
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.terminated {
		return false, contract.NewError(contract.ErrorSessionStopping, "session has terminated")
	}
	if _, exists := store.pending[request.ID]; exists {
		return false, invariantf("duplicate extension UI dialog ID %q", request.ID)
	}
	store.pending[request.ID] = question
	return true, nil
}

func (store *QuestionStore) Summaries() []QuestionSummary {
	store.mu.Lock()
	defer store.mu.Unlock()

	summaries := make([]QuestionSummary, 0, len(store.pending))
	for _, question := range store.pending {
		if question.answering {
			continue
		}
		summary := question.summary
		summary.Options = append([]string(nil), summary.Options...)
		summaries = append(summaries, summary)
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].ID < summaries[j].ID })
	return summaries
}

func (store *QuestionStore) Answer(ctx context.Context, id string, raw json.RawMessage) error {
	store.mu.Lock()
	question, ok := store.pending[id]
	if !ok {
		store.mu.Unlock()
		return contract.NewError(contract.ErrorNotFound, "pending question not found")
	}
	if question.answering {
		store.mu.Unlock()
		return contract.NewError(contract.ErrorConflict, "question is already being answered")
	}
	response, err := buildQuestionResponse(id, question.summary.Method, raw)
	if err != nil {
		store.mu.Unlock()
		return err
	}
	if store.sender == nil {
		store.mu.Unlock()
		return invariantf("question store has no Pi RPC sender")
	}
	question.answering = true
	store.mu.Unlock()

	err = store.sender.Send(ctx, response)

	store.mu.Lock()
	defer store.mu.Unlock()
	if current := store.pending[id]; current == question {
		if err == nil || store.terminated {
			delete(store.pending, id)
		} else {
			question.answering = false
		}
	}
	return err
}

func (store *QuestionStore) Clear() {
	store.mu.Lock()
	store.terminated = true
	store.pending = make(map[string]*pendingQuestion)
	store.mu.Unlock()
}

func buildQuestionResponse(id, method string, raw json.RawMessage) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var answer questionAnswer
	if err := decoder.Decode(&answer); err != nil {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "invalid question answer: "+err.Error())
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "invalid question answer: "+err.Error())
	}

	provided := 0
	if answer.Value != nil {
		provided++
	}
	if answer.Confirmed != nil {
		provided++
	}
	if answer.Cancelled {
		provided++
	}
	if provided != 1 {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "question answer must provide exactly one response")
	}

	response := pirpc.ExtensionUIResponse{Type: "extension_ui_response", ID: id}
	switch {
	case answer.Cancelled:
		response.Cancelled = true
	case method == "confirm" && answer.Confirmed != nil:
		response.Confirmed = answer.Confirmed
	case method != "confirm" && answer.Value != nil:
		response.Value = answer.Value
	default:
		return nil, contract.NewError(contract.ErrorInvalidRequest, "question answer does not match dialog method")
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode extension UI response: %w", err)
	}
	return encoded, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func isDialogMethod(method string) bool {
	switch method {
	case "select", "confirm", "input", "editor":
		return true
	default:
		return false
	}
}
