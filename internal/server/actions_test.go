package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

// effectiveAddrForTests is the address used in mustNewHandlerWithFleetAuth.
const effectiveAddrForTests = "127.0.0.1:8080"

// serveActionRequest sends a write-boundary-compliant POST request to the handler.
func serveActionRequest(t *testing.T, handler http.Handler, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = effectiveAddrForTests
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// TestSteerActionPatchesDeckStatus verifies that a successful steer call
// patches #deck-status with a success indicator.
func TestSteerActionPatchesDeckStatus(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	resp := serveActionRequest(t, handler, "/ui/sessions/sess-1/steer", `{}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("steer status = %d, want %d; body = %q", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "deck-status") {
		t.Errorf("steer response does not patch #deck-status:\n%s", body)
	}
}

// TestInterruptActionPatchesDeckStatus verifies that an interrupt action
// patches #deck-status.
func TestInterruptActionPatchesDeckStatus(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	resp := serveActionRequest(t, handler, "/ui/sessions/sess-1/interrupt", `{}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("interrupt status = %d, want %d; body = %q", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "deck-status") {
		t.Errorf("interrupt response does not patch #deck-status:\n%s", body)
	}
}

// TestStopActionPatchesDeckStatus verifies that a stop session action
// patches #deck-status.
func TestStopActionPatchesDeckStatus(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	resp := serveActionRequest(t, handler, "/ui/sessions/sess-1/stop", `{}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("stop status = %d, want %d; body = %q", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "deck-status") {
		t.Errorf("stop response does not patch #deck-status:\n%s", body)
	}
}

// TestNewSessionActionPatchesDeckStatus verifies that a new session action
// patches #deck-status.
func TestNewSessionActionPatchesDeckStatus(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	resp := serveActionRequest(t, handler, "/ui/sessions", `{}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("new session status = %d, want %d; body = %q", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "deck-status") {
		t.Errorf("new session response does not patch #deck-status:\n%s", body)
	}
}

// TestAnswerQuestionActionPatchesDeckStatus verifies that answering a question
// patches #deck-status.
func TestAnswerQuestionActionPatchesDeckStatus(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	resp := serveActionRequest(t, handler, "/ui/sessions/sess-1/questions/q-1", `{"value":"my answer"}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("answer question status = %d, want %d; body = %q", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "deck-status") {
		t.Errorf("answer question response does not patch #deck-status:\n%s", body)
	}
}

// TestActionErrorMapToStableOperatorCopy verifies that contract errors are
// mapped to stable operator-facing copy without leaking internal details.
func TestActionErrorMapToStableOperatorCopy(t *testing.T) {
	tests := []struct {
		code    contract.ErrorCode
		wantNot []string // must NOT appear in output
	}{
		{contract.ErrorSessionStopping, []string{"session_stopping"}},
		{contract.ErrorNotFound, []string{"not_found"}},
		{contract.ErrorSaturated, []string{"saturated"}},
		{contract.ErrorConflict, []string{"conflict"}},
		{contract.ErrorInternal, []string{"internal"}},
	}

	for _, tt := range tests {
		err := contract.NewError(tt.code, "raw internal detail that must not leak")
		msg := operatorMessage(err)
		if msg == "" {
			t.Errorf("code %q: operatorMessage returned empty string", tt.code)
		}
		for _, forbidden := range tt.wantNot {
			if strings.Contains(msg, forbidden) {
				t.Errorf("code %q: operatorMessage leaked internal code %q in message %q", tt.code, forbidden, msg)
			}
		}
		if strings.Contains(msg, "raw internal detail") {
			t.Errorf("code %q: operatorMessage leaked raw internal error detail: %q", tt.code, msg)
		}
	}
}

// TestOperatorMessageNilError returns empty string for nil error.
func TestOperatorMessageNilError(t *testing.T) {
	if msg := operatorMessage(nil); msg != "" {
		t.Errorf("operatorMessage(nil) = %q, want empty", msg)
	}
}

// TestActionRequiresWriteBoundary verifies that action routes reject requests
// that fail the write boundary check.
func TestActionRequiresWriteBoundary(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	// Missing Content-Type → 415.
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/sess-1/steer", strings.NewReader(`{}`))
	req.Host = effectiveAddrForTests
	// Deliberately omit Content-Type.
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("missing Content-Type: status = %d, want %d", w.Code, http.StatusUnsupportedMediaType)
	}

	// Wrong Host → 403.
	req2 := httptest.NewRequest(http.MethodPost, "/ui/sessions/sess-1/steer", strings.NewReader(`{}`))
	req2.Host = "attacker.example.com"
	req2.Header.Set("Content-Type", "application/json")
	req2.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Errorf("wrong Host: status = %d, want %d", w2.Code, http.StatusForbidden)
	}
}

// TestActionUnauthenticated verifies that action routes require a session cookie.
func TestActionUnauthenticated(t *testing.T) {
	fleet := newStreamFakeFleet()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler, err := newHandlerWithOptions(logger, effectiveAddrForTests, &bytes.Buffer{}, fleet, ctx)
	if err != nil {
		t.Fatalf("newHandlerWithOptions: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/sess-1/steer", strings.NewReader(`{}`))
	req.Host = effectiveAddrForTests
	req.Header.Set("Content-Type", "application/json")
	// No cookie.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated action: status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// TestSteerActionWithNoFleetReturnsNotFound verifies that action routes when
// no fleet manager is configured return 404 (stub handler).
func TestSteerActionWithNoFleetReturnsNotFound(t *testing.T) {
	// This tests the nil-fleet handler path.
	handler, cookie := mustNewHandlerWithAuth(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	// With nil fleet the POST routes are stub NotFound handlers — but the
	// write boundary still applies.
	req := httptest.NewRequest(http.MethodPost, "/ui/sessions/sess-1/steer", strings.NewReader(`{}`))
	req.Host = "127.0.0.1:0" // matches what mustNewHandlerWithAuth uses
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("no-fleet steer: status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// errFleet is a fake that always returns errors from mutation methods.
type errFleet struct {
	*streamFakeFleet
	mutationErr error
}

func (e *errFleet) Steer(context.Context, string, string) error { return e.mutationErr }
func (e *errFleet) Interrupt(context.Context, string) error     { return e.mutationErr }
func (e *errFleet) StopSession(context.Context, string) error   { return e.mutationErr }
func (e *errFleet) SpawnRoot(context.Context) (string, error)   { return "", e.mutationErr }
func (e *errFleet) AnswerQuestion(context.Context, string, string, json.RawMessage) error {
	return e.mutationErr
}

// TestActionErrorPatchesDeckStatusWithMessage verifies that when a manager
// call returns an error, the response patches #deck-status with operator copy.
func TestActionErrorPatchesDeckStatusWithMessage(t *testing.T) {
	contractErr := contract.NewError(contract.ErrorSessionStopping, "internal detail")
	ef := &errFleet{
		streamFakeFleet: newStreamFakeFleet(),
		mutationErr:     contractErr,
	}
	handler, cookie := mustNewHandlerWithFleetAuth(t, ef)

	resp := serveActionRequest(t, handler, "/ui/sessions/sess-1/steer", `{}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("steer error status = %d, want %d; body = %q", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "deck-status") {
		t.Errorf("steer error response does not patch #deck-status:\n%s", body)
	}
	// Must not leak internal error detail.
	if strings.Contains(body, "internal detail") {
		t.Errorf("steer error response leaked internal error detail:\n%s", body)
	}
	if strings.Contains(body, "session_stopping") {
		t.Errorf("steer error response leaked contract error code:\n%s", body)
	}
}

// TestErrorFromNonContractErrorPatchesDeckStatusWithGenericMessage verifies
// that non-contract errors also produce stable generic operator copy.
func TestErrorFromNonContractErrorPatchesDeckStatusWithGenericMessage(t *testing.T) {
	ef := &errFleet{
		streamFakeFleet: newStreamFakeFleet(),
		mutationErr:     errors.New("internal details that must not leak"),
	}
	handler, cookie := mustNewHandlerWithFleetAuth(t, ef)

	resp := serveActionRequest(t, handler, "/ui/sessions/sess-1/steer", `{}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("generic error status = %d, want %d; body = %q", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()
	if !strings.Contains(body, "deck-status") {
		t.Errorf("generic error response does not patch #deck-status:\n%s", body)
	}
	if strings.Contains(body, "internal details that must not leak") {
		t.Errorf("generic error response leaked internal error detail:\n%s", body)
	}
}
