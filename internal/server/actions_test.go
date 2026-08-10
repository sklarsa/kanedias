package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sklarsa/kanedias/internal/manager"
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

// TestSteerActionPatchesCompleteDeckStatusWrapperWithOuterMode verifies the
// deck-status patch carries the complete #deck-status root element in outer
// mode. renderTemplate returns the full wrapper, so inner mode would nest that
// wrapper under its own same-ID root and throw a HierarchyRequestError (no
// status renders). Pinning the exact payload+mode pairing catches that
// regression end to end.
func TestSteerActionPatchesCompleteDeckStatusWrapperWithOuterMode(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	resp := serveActionRequest(t, handler, "/ui/sessions/sess-1/steer", `{}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("steer status = %d, want %d; body = %q", resp.Code, http.StatusOK, resp.Body.String())
	}
	body := resp.Body.String()

	// The payload must target #deck-status and carry the complete wrapper root,
	// not just an inner fragment.
	if !strings.Contains(body, "selector #deck-status") {
		t.Errorf("steer patch does not select #deck-status:\n%s", body)
	}
	if !strings.Contains(body, `<div id="deck-status"`) {
		t.Errorf("steer patch does not carry the complete #deck-status wrapper:\n%s", body)
	}
	// The successful acknowledgment must be present.
	if !strings.Contains(body, "Command sent.") {
		t.Errorf("steer patch missing success copy:\n%s", body)
	}
	// Outer mode is Datastar's default. Inner mode must NOT be requested,
	// because inner mode nests the complete wrapper under its own same-ID root
	// (HierarchyRequestError on the client).
	if strings.Contains(body, "mode inner") {
		t.Errorf("steer patch uses inner mode, which would nest the #deck-status wrapper under itself (HierarchyRequestError):\n%s", body)
	}
}

func TestRenameRootActionValidClearAndStrictSignals(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	response := serveActionRequest(t, handler, "/ui/sessions/root-1/name", `{"name":"new name"}`, cookie)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "deck-status") {
		t.Fatalf("valid rename status = %d, body = %q", response.Code, response.Body.String())
	}
	if fleet.renameSessionID != "root-1" || fleet.renameName != "new name" {
		t.Fatalf("RenameRoot called with (%q, %q), want (%q, %q)", fleet.renameSessionID, fleet.renameName, "root-1", "new name")
	}

	response = serveActionRequest(t, handler, "/ui/sessions/root-1/name", `{"name":""}`, cookie)
	if response.Code != http.StatusOK || fleet.renameName != "" {
		t.Fatalf("clear rename status = %d, recorded name = %q", response.Code, fleet.renameName)
	}

	fleet.renameName = "not-called"
	for _, body := range []string{`{"name":`, `{"name":"ok","extra":true}`, `{"name":"ok"} {}`} {
		response = serveActionRequest(t, handler, "/ui/sessions/root-1/name", body, cookie)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "deck-status") {
			t.Fatalf("malformed rename body %q status = %d, body = %q", body, response.Code, response.Body.String())
		}
		if fleet.renameName != "not-called" {
			t.Fatalf("malformed rename invoked manager with %q", fleet.renameName)
		}
		if strings.Contains(response.Body.String(), "unexpected") || strings.Contains(response.Body.String(), "invalid character") {
			t.Fatalf("malformed rename leaked decoder detail: %s", response.Body.String())
		}
	}
}

func TestRenameRootActionRejectsDescendantAndSanitizesManagerFailure(t *testing.T) {
	for _, failure := range []error{
		contract.NewError(contract.ErrorInvalidRequest, "descendant child-1 is private"),
		errors.New("private manager failure /run/secret.sock"),
	} {
		var logs bytes.Buffer
		fleet := newStreamFakeFleet()
		fleet.renameErr = failure
		handler, cookie := mustNewHandlerWithFleetAuthLogger(t, fleet, slog.New(slog.NewTextHandler(&logs, nil)))
		response := serveActionRequest(t, handler, "/ui/sessions/child-1/name", `{"name":"Nope"}`, cookie)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "deck-status") {
			t.Fatalf("failure status = %d, body = %q", response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), failure.Error()) || strings.Contains(response.Body.String(), "child-1 is private") || strings.Contains(response.Body.String(), "/run/secret.sock") {
			t.Fatalf("response leaked manager failure: %s", response.Body.String())
		}
		if !strings.Contains(logs.String(), failure.Error()) {
			t.Fatalf("server log missing manager failure %q: %s", failure, logs.String())
		}
	}
}

func TestRenameRootActionSecurityAndDisabledRoute(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	unauthenticated := httptest.NewRequest(http.MethodPost, "/ui/sessions/root-1/name", strings.NewReader(`{"name":"x"}`))
	unauthenticated.Host = effectiveAddrForTests
	unauthenticated.Header.Set("Content-Type", "application/json")
	unauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rename status = %d, want 401", unauthenticatedResponse.Code)
	}

	crossOrigin := httptest.NewRequest(http.MethodPost, "/ui/sessions/root-1/name", strings.NewReader(`{"name":"x"}`))
	crossOrigin.Host = effectiveAddrForTests
	crossOrigin.Header.Set("Content-Type", "application/json")
	crossOrigin.Header.Set("Origin", "http://attacker.example")
	crossOrigin.AddCookie(cookie)
	crossOriginResponse := httptest.NewRecorder()
	handler.ServeHTTP(crossOriginResponse, crossOrigin)
	if crossOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-origin rename status = %d, want 403", crossOriginResponse.Code)
	}

	disabled, disabledCookie := mustNewHandlerWithAuth(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	disabledRequest := httptest.NewRequest(http.MethodPost, "/ui/sessions/root-1/name", strings.NewReader(`{"name":"x"}`))
	disabledRequest.Host = "127.0.0.1:0"
	disabledRequest.Header.Set("Content-Type", "application/json")
	disabledRequest.AddCookie(disabledCookie)
	disabledResponse := httptest.NewRecorder()
	disabled.ServeHTTP(disabledResponse, disabledRequest)
	if disabledResponse.Code != http.StatusNotFound {
		t.Fatalf("disabled rename status = %d, want 404", disabledResponse.Code)
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

func TestNewSessionActionAcceptsStrictJSONAndReturnsCreatedSession(t *testing.T) {
	fleet := newStreamFakeFleet()
	fleet.launchOptions = launchOptionsFixture()
	fleet.spawnSessionID = "session-created"
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	body := `{"name":"release triage","repository":"one/alpha","root":{"modelType":"deep-model","thinkingLevel":"xhigh"},"workers":[{"workerType":"oracle","modelType":"deep-model","thinkingLevel":"high"},{"workerType":"reviewer","modelType":"fast-model","thinkingLevel":"medium"},{"workerType":"worker","modelType":"deep-model","thinkingLevel":"xhigh"}]}`

	resp := serveActionRequest(t, handler, "/ui/sessions", body, cookie)
	if resp.Code != http.StatusCreated {
		t.Fatalf("new session status = %d, want %d; body = %q", resp.Code, http.StatusCreated, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := strings.TrimSpace(resp.Body.String()); got != `{"sessionId":"session-created"}` {
		t.Fatalf("response = %q", got)
	}
	want := manager.SessionLaunchRequest{
		Name:       "release triage",
		Repository: "one/alpha",
		Root:       manager.ModelSelection{ModelType: "deep-model", ThinkingLevel: "xhigh"},
		Workers: []manager.WorkerModelSelection{
			{WorkerType: "oracle", ModelType: "deep-model", ThinkingLevel: "high"},
			{WorkerType: "reviewer", ModelType: "fast-model", ThinkingLevel: "medium"},
			{WorkerType: "worker", ModelType: "deep-model", ThinkingLevel: "xhigh"},
		},
	}
	if !reflect.DeepEqual(fleet.spawnRequest, want) {
		t.Fatalf("recorded request = %#v, want %#v", fleet.spawnRequest, want)
	}
}

func TestNewSessionActionRejectsInvalidJSON(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed", body: `{"root":`},
		{name: "unknown field", body: `{"root":{},"workers":[],"unexpected":true}`},
		{name: "trailing value", body: `{"root":{},"workers":[]} {}`},
		{name: "oversize", body: `{"root":{},"workers":[` + strings.Repeat(`{"workerType":"worker","modelType":"deep-model","thinkingLevel":"high"},`, 1000) + `{}]}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fleet := newStreamFakeFleet()
			handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
			resp := serveActionRequest(t, handler, "/ui/sessions", test.body, cookie)
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %q", resp.Code, resp.Body.String())
			}
			if got := strings.TrimSpace(resp.Body.String()); got != `{"error":"The session configuration was not valid."}` {
				t.Fatalf("response = %q", got)
			}
			if strings.Contains(resp.Body.String(), "unexpected") || strings.Contains(resp.Body.String(), "invalid character") {
				t.Errorf("response leaked decoder detail: %s", resp.Body.String())
			}
		})
	}
}

func TestNewSessionActionPreservesAuthenticationAndSameOriginBoundary(t *testing.T) {
	fleet := newStreamFakeFleet()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	var bootstrap bytes.Buffer
	handler, err := newHandlerWithOptions(logger, effectiveAddrForTests, &bootstrap, fleet, context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"root":{},"workers":[]}`

	unauthenticated := httptest.NewRequest(http.MethodPost, "/ui/sessions", strings.NewReader(body))
	unauthenticated.Host = effectiveAddrForTests
	unauthenticated.Header.Set("Content-Type", "application/json")
	unauthenticatedResponse := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedResponse, unauthenticated)
	if unauthenticatedResponse.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", unauthenticatedResponse.Code)
	}

	// Use a handler/cookie pair from the same capability store for the boundary check.
	boundaryHandler, boundaryCookie := mustNewHandlerWithFleetAuth(t, fleet)
	crossOrigin := httptest.NewRequest(http.MethodPost, "/ui/sessions", strings.NewReader(body))
	crossOrigin.Host = effectiveAddrForTests
	crossOrigin.Header.Set("Content-Type", "application/json")
	crossOrigin.Header.Set("Origin", "http://attacker.example")
	crossOrigin.AddCookie(boundaryCookie)
	crossOriginResponse := httptest.NewRecorder()
	boundaryHandler.ServeHTTP(crossOriginResponse, crossOrigin)
	if crossOriginResponse.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", crossOriginResponse.Code)
	}
}

func TestNewSessionActionMapsTypedInvalidAndSpawnFailure(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
	}{
		{name: "invalid request", err: contract.NewError(contract.ErrorInvalidRequest, "unknown private model"), wantStatus: http.StatusBadRequest, wantBody: `{"error":"The session configuration was not valid."}`},
		{name: "repository unavailable", err: contract.NewError(contract.ErrorWorkspaceRepositoryUnavailable, "/private/path"), wantStatus: http.StatusServiceUnavailable, wantBody: `{"error":"The selected repository is not present in the workspace."}`},
		{name: "admission failure", err: errors.New("private socket path /run/secret.sock"), wantStatus: http.StatusServiceUnavailable, wantBody: `{"error":"The session could not be started."}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			fleet := newStreamFakeFleet()
			fleet.spawnErr = test.err
			handler, cookie := mustNewHandlerWithFleetAuthLogger(t, fleet, slog.New(slog.NewTextHandler(&logs, nil)))
			resp := serveActionRequest(t, handler, "/ui/sessions", `{"root":{},"workers":[]}`, cookie)
			if resp.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %q", resp.Code, test.wantStatus, resp.Body.String())
			}
			if got := strings.TrimSpace(resp.Body.String()); got != test.wantBody {
				t.Fatalf("response = %q, want %q", got, test.wantBody)
			}
			if strings.Contains(resp.Body.String(), test.err.Error()) || strings.Contains(resp.Body.String(), "/private/path") {
				t.Errorf("response leaked real error: %s", resp.Body.String())
			}
			if !strings.Contains(logs.String(), test.err.Error()) {
				t.Errorf("server log does not contain real error: %s", logs.String())
			}
		})
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
	handler, err := newHandlerWithOptions(logger, effectiveAddrForTests, &bytes.Buffer{}, fleet, ctx, true)
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

func (e *errFleet) RenameRoot(string, string) error             { return e.mutationErr }
func (e *errFleet) Steer(context.Context, string, string) error { return e.mutationErr }
func (e *errFleet) Interrupt(context.Context, string) error     { return e.mutationErr }
func (e *errFleet) StopSession(context.Context, string) error   { return e.mutationErr }
func (e *errFleet) SpawnRoot(context.Context) (string, error)   { return "", e.mutationErr }
func (e *errFleet) SpawnRootWithRequest(context.Context, manager.SessionLaunchRequest) (string, error) {
	return "", e.mutationErr
}
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

// TestActionErrorIsLoggedServerSide verifies that a failed manager command
// records its real (unsanitized) error server-side even though the operator
// only sees generic copy — otherwise failures are undiagnosable.
func TestActionErrorIsLoggedServerSide(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))
	ef := &errFleet{
		streamFakeFleet: newStreamFakeFleet(),
		mutationErr:     errors.New("root is not actionable: real cause"),
	}
	handler, cookie := mustNewHandlerWithFleetAuthLogger(t, ef, logger)

	resp := serveActionRequest(t, handler, "/ui/sessions/sess-1/steer", `{}`, cookie)
	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	// Browser must NOT see the real cause.
	if strings.Contains(resp.Body.String(), "real cause") {
		t.Errorf("response leaked real cause to browser:\n%s", resp.Body.String())
	}
	// Server log MUST record the real cause with request context.
	logged := logBuf.String()
	if !strings.Contains(logged, "real cause") {
		t.Errorf("server log did not record the real error cause:\n%s", logged)
	}
	if !strings.Contains(logged, "/ui/sessions/sess-1/steer") {
		t.Errorf("server log did not record the request path:\n%s", logged)
	}
}
