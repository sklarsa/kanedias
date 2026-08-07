package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor/pirpc"
)

func TestQuestionStoreParsesDocumentedPiDialogFixtures(t *testing.T) {
	store := NewQuestionStore(nil)
	fixtures := []string{"pi-select.json", "pi-confirm.json", "pi-input.json", "pi-editor.json"}
	for _, fixture := range fixtures {
		retained, err := store.Retain(readQuestionFixture(t, fixture))
		if err != nil {
			t.Fatalf("Retain(%s) error = %v", fixture, err)
		}
		if !retained {
			t.Fatalf("Retain(%s) = false, want blocking dialog retained", fixture)
		}
	}

	want := []QuestionSummary{
		{ID: "uuid-1", Method: "select", Title: "Allow dangerous command?", Options: []string{"Allow", "Block"}, Timeout: 10000},
		{ID: "uuid-2", Method: "confirm", Title: "Clear session?", Message: "All messages will be lost.", Timeout: 5000},
		{ID: "uuid-3", Method: "input", Title: "Enter a value", Placeholder: "type something..."},
		{ID: "uuid-4", Method: "editor", Title: "Edit some text", Prefill: "Line 1\nLine 2\nLine 3"},
	}
	if got := store.Summaries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Summaries() = %#v, want %#v", got, want)
	}
}

func TestQuestionStoreDoesNotRetainFireAndForgetRequests(t *testing.T) {
	store := NewQuestionStore(nil)
	retained, err := store.Retain(json.RawMessage(`{"type":"extension_ui_request","id":"uuid-5","method":"notify","message":"done"}`))
	if err != nil {
		t.Fatalf("Retain(notify) error = %v", err)
	}
	if retained {
		t.Fatal("Retain(notify) = true, want false")
	}
	if got := store.Summaries(); len(got) != 0 {
		t.Fatalf("Summaries() length = %d, want 0", len(got))
	}
}

func TestQuestionStoreSurvivesEventRingEviction(t *testing.T) {
	store := NewQuestionStore(nil)
	if retained, err := store.Retain(readQuestionFixture(t, "pi-select.json")); err != nil || !retained {
		t.Fatalf("Retain(select) = (%v, %v), want (true, nil)", retained, err)
	}
	broker := newEventBroker(1, 1)
	broker.PublishLocal("root", "pi", json.RawMessage(`{"event":1}`))
	broker.PublishLocal("root", "pi", json.RawMessage(`{"event":2}`))

	if got := store.Summaries(); len(got) != 1 || got[0].ID != "uuid-1" {
		t.Fatalf("questions after ring eviction = %#v, want uuid-1 pending", got)
	}
}

func TestQuestionStoreSendsExactResponsesAndConsumesOnce(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		answer   string
		wantWire string
	}{
		{name: "value", fixture: "pi-select.json", answer: `{"value":"Allow"}`, wantWire: `{"type":"extension_ui_response","id":"uuid-1","value":"Allow"}`},
		{name: "confirmed", fixture: "pi-confirm.json", answer: `{"confirmed":true}`, wantWire: `{"type":"extension_ui_response","id":"uuid-2","confirmed":true}`},
		{name: "cancelled", fixture: "pi-input.json", answer: `{"cancelled":true}`, wantWire: `{"type":"extension_ui_response","id":"uuid-3","cancelled":true}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConn, peerConn := net.Pipe()
			rpc := pirpc.NewClient(clientConn)
			defer rpc.Close()
			defer peerConn.Close()
			store := NewQuestionStore(rpc)
			if retained, err := store.Retain(readQuestionFixture(t, tt.fixture)); err != nil || !retained {
				t.Fatalf("Retain() = (%v, %v), want (true, nil)", retained, err)
			}

			wire := make(chan string, 1)
			go func() {
				line, _ := bufio.NewReader(peerConn).ReadString('\n')
				wire <- line
			}()
			if err := store.Answer(context.Background(), questionIDForFixture(tt.fixture), json.RawMessage(tt.answer)); err != nil {
				t.Fatalf("Answer() error = %v", err)
			}
			select {
			case got := <-wire:
				if got != tt.wantWire+"\n" {
					t.Fatalf("wire response = %q, want %q", got, tt.wantWire+"\n")
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for extension UI response")
			}
			if got := store.Summaries(); len(got) != 0 {
				t.Fatalf("Summaries() after answer = %#v, want empty", got)
			}
			if err := store.Answer(context.Background(), questionIDForFixture(tt.fixture), json.RawMessage(tt.answer)); err == nil {
				t.Fatal("second Answer() error = nil, want stale answer rejected")
			}
		})
	}
}

func TestQuestionStoreRejectsConcurrentSecondAnswerWhileFirstSendIsPending(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	rpc := pirpc.NewClient(clientConn)
	defer rpc.Close()
	defer peerConn.Close()
	store := NewQuestionStore(rpc)
	if retained, err := store.Retain(readQuestionFixture(t, "pi-select.json")); err != nil || !retained {
		t.Fatalf("Retain() = (%v, %v), want (true, nil)", retained, err)
	}

	first := make(chan error, 1)
	go func() {
		first <- store.Answer(context.Background(), "uuid-1", json.RawMessage(`{"value":"Allow"}`))
	}()
	waitFor(t, func() bool { return len(store.Summaries()) == 0 }, "first answer to reserve pending question")
	if err := store.Answer(context.Background(), "uuid-1", json.RawMessage(`{"value":"Block"}`)); err == nil {
		t.Fatal("concurrent second Answer() error = nil, want conflict")
	}

	line, err := bufio.NewReader(peerConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read first extension UI response: %v", err)
	}
	if line != `{"type":"extension_ui_response","id":"uuid-1","value":"Allow"}`+"\n" {
		t.Fatalf("wire response = %q, want first answer only", line)
	}
	if err := <-first; err != nil {
		t.Fatalf("first Answer() error = %v", err)
	}
}

func TestQuestionStoreRejectsAnswersThatDoNotMatchDialogMethod(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		answer  string
	}{
		{name: "select cannot be confirmed", fixture: "pi-select.json", answer: `{"confirmed":true}`},
		{name: "confirm cannot carry value", fixture: "pi-confirm.json", answer: `{"value":"yes"}`},
		{name: "answer cannot carry multiple fields", fixture: "pi-input.json", answer: `{"value":"text","cancelled":true}`},
		{name: "answer cannot be empty", fixture: "pi-editor.json", answer: `{}`},
		{name: "answer rejects unknown field", fixture: "pi-input.json", answer: `{"value":"text","extra":true}`},
		{name: "answer rejects trailing JSON", fixture: "pi-input.json", answer: `{"value":"text"}{}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewQuestionStore(nil)
			if retained, err := store.Retain(readQuestionFixture(t, tt.fixture)); err != nil || !retained {
				t.Fatalf("Retain() = (%v, %v), want (true, nil)", retained, err)
			}
			if err := store.Answer(context.Background(), questionIDForFixture(tt.fixture), json.RawMessage(tt.answer)); err == nil {
				t.Fatal("Answer(invalid) error = nil")
			}
			if got := store.Summaries(); len(got) != 1 {
				t.Fatalf("pending questions after invalid answer = %#v, want original question", got)
			}
		})
	}
}

func TestQuestionStoreRestoresPendingQuestionWhenPiSendFails(t *testing.T) {
	clientConn, peerConn := net.Pipe()
	rpc := pirpc.NewClient(clientConn)
	peerConn.Close()
	defer rpc.Close()

	store := NewQuestionStore(rpc)
	if retained, err := store.Retain(readQuestionFixture(t, "pi-select.json")); err != nil || !retained {
		t.Fatalf("Retain() = (%v, %v), want (true, nil)", retained, err)
	}
	if err := store.Answer(context.Background(), "uuid-1", json.RawMessage(`{"value":"Allow"}`)); err == nil {
		t.Fatal("Answer() error = nil, want Pi send failure")
	}
	if got := store.Summaries(); len(got) != 1 || got[0].ID != "uuid-1" {
		t.Fatalf("Summaries() after send failure = %#v, want uuid-1 restored", got)
	}
}

func TestQuestionStoreClearDropsQuestionsAndRejectsNewOnes(t *testing.T) {
	store := NewQuestionStore(nil)
	if retained, err := store.Retain(readQuestionFixture(t, "pi-editor.json")); err != nil || !retained {
		t.Fatalf("Retain() = (%v, %v), want (true, nil)", retained, err)
	}
	store.Clear()
	if got := store.Summaries(); len(got) != 0 {
		t.Fatalf("Summaries() after Clear = %#v, want empty", got)
	}
	if retained, err := store.Retain(readQuestionFixture(t, "pi-input.json")); err == nil || retained {
		t.Fatalf("Retain() after Clear = (%v, %v), want (false, error)", retained, err)
	}
}

func readQuestionFixture(t *testing.T, name string) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func questionIDForFixture(name string) string {
	switch name {
	case "pi-select.json":
		return "uuid-1"
	case "pi-confirm.json":
		return "uuid-2"
	case "pi-input.json":
		return "uuid-3"
	default:
		return "uuid-4"
	}
}

func TestQuestionStoreExpiresPiTimeoutAndCanCancelGenerationDialogs(t *testing.T) {
	store := NewQuestionStore(nil)
	retained, err := store.Retain(json.RawMessage(`{"type":"extension_ui_request","id":"timed","method":"input","title":"value","timeout":5}`))
	if err != nil || !retained {
		t.Fatalf("Retain() = %v, %v", retained, err)
	}
	waitFor(t, func() bool { return len(store.Summaries()) == 0 }, "question timeout expiry")
	retained, err = store.Retain(json.RawMessage(`{"type":"extension_ui_request","id":"aborted","method":"confirm","title":"continue?"}`))
	if err != nil || !retained {
		t.Fatalf("Retain() = %v, %v", retained, err)
	}
	store.CancelPending()
	if got := store.Summaries(); len(got) != 0 {
		t.Fatalf("pending after cancellation = %#v", got)
	}
	retained, err = store.Retain(json.RawMessage(`{"type":"extension_ui_request","id":"next","method":"input","title":"next"}`))
	if err != nil || !retained {
		t.Fatalf("store did not accept next generation: %v, %v", retained, err)
	}
}
