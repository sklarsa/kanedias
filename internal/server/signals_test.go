package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDecodeSignalsRejectsUnknownFields(t *testing.T) {
	body := `{"message":"hello","unknown":"field"}`
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/x/steer",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	_, err := decodeSignals[steerSignals](w, req)
	if err == nil {
		t.Fatal("decodeSignals accepted unknown field, want error")
	}
}

func TestDecodeSignalsRejectsTrailingJSON(t *testing.T) {
	// Two JSON objects concatenated.
	body := `{"message":"hello"}{"message":"extra"}`
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/x/steer",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	_, err := decodeSignals[steerSignals](w, req)
	if err == nil {
		t.Fatal("decodeSignals accepted trailing JSON, want error")
	}
}

func TestDecodeSignalsSteerMessage(t *testing.T) {
	body := `{"message":"focus on the tests"}`
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/x/steer",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	got, err := decodeSignals[steerSignals](w, req)
	if err != nil {
		t.Fatalf("decodeSignals error = %v", err)
	}
	if got.Message != "focus on the tests" {
		t.Errorf("message = %q, want %q", got.Message, "focus on the tests")
	}
}

func TestDecodeSignalsSelectedSession(t *testing.T) {
	body := `{"selectedSessionId":"abc-123"}`
	target := "/ui/session?datastar=" + url.QueryEscape(body)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()

	got, err := decodeSignals[selectedSessionSignals](w, req)
	if err != nil {
		t.Fatalf("decodeSignals error = %v", err)
	}
	if got.SelectedSessionID != "abc-123" {
		t.Errorf("selectedSessionId = %q, want abc-123", got.SelectedSessionID)
	}
}

func TestDecodeSignalsAnswerValue(t *testing.T) {
	body := `{"value":"selected option"}`
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/x/questions/q1",
		strings.NewReader(body))
	w := httptest.NewRecorder()

	got, err := decodeSignals[answerSignals](w, req)
	if err != nil {
		t.Fatalf("decodeSignals error = %v", err)
	}
	if got.Value == nil || *got.Value != "selected option" {
		t.Errorf("value = %v, want selected option", got.Value)
	}
	if got.Confirmed != nil {
		t.Errorf("confirmed = %v, want nil", got.Confirmed)
	}
}

func TestDecodeSignalsAnswerConfirmed(t *testing.T) {
	body := `{"confirmed":true}`
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/x/questions/q1",
		strings.NewReader(body))
	w := httptest.NewRecorder()

	got, err := decodeSignals[answerSignals](w, req)
	if err != nil {
		t.Fatalf("decodeSignals error = %v", err)
	}
	if got.Confirmed == nil || !*got.Confirmed {
		t.Errorf("confirmed = %v, want true", got.Confirmed)
	}
}

func TestDecodeSignalsAnswerCancelled(t *testing.T) {
	body := `{"cancelled":true}`
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/x/questions/q1",
		strings.NewReader(body))
	w := httptest.NewRecorder()

	got, err := decodeSignals[answerSignals](w, req)
	if err != nil {
		t.Fatalf("decodeSignals error = %v", err)
	}
	if !got.Cancelled {
		t.Error("cancelled = false, want true")
	}
}

func TestDecodeSignalsCapsBodyAt64KiB(t *testing.T) {
	// Build a body larger than 64 KiB.
	large := `{"message":"` + strings.Repeat("x", 65*1024) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/x/steer",
		strings.NewReader(large))
	w := httptest.NewRecorder()

	_, err := decodeSignals[steerSignals](w, req)
	if err == nil {
		t.Fatal("decodeSignals accepted oversized body, want error")
	}
}
