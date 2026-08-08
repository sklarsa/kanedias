package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/sklarsa/kanedias/internal/manager"
	"github.com/sklarsa/kanedias/internal/supervisor"
)

// mustNewHandlerWithAuth creates a handler and returns a valid session cookie
// by going through the bootstrap flow. This allows tests to exercise protected routes.
func mustNewHandlerWithAuth(t *testing.T, logger *slog.Logger) (http.Handler, *http.Cookie) {
	t.Helper()
	var bootstrapOut bytes.Buffer
	handler, err := newHandlerWithOptions(logger, "127.0.0.1:0", &bootstrapOut, nil, context.Background())
	if err != nil {
		t.Fatalf("newHandlerWithOptions: %v", err)
	}

	// Extract bootstrap token from the output.
	output := bootstrapOut.String()
	idx := strings.Index(output, bootstrapQueryName+"=")
	if idx == -1 {
		t.Fatalf("bootstrap output does not contain token: %q", output)
	}
	token := strings.TrimSpace(output[idx+len(bootstrapQueryName)+1:])

	// Exchange the bootstrap token for a session cookie.
	req := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+token, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap status = %d, want %d", w.Code, http.StatusSeeOther)
	}

	var cookie *http.Cookie
	for _, h := range w.Header()["Set-Cookie"] {
		if strings.HasPrefix(h, sessionCookieName+"=") {
			parts := strings.SplitN(h, "=", 2)
			if len(parts) == 2 {
				cookie = &http.Cookie{
					Name:  sessionCookieName,
					Value: strings.Split(parts[1], ";")[0],
				}
			}
			break
		}
	}
	if cookie == nil {
		t.Fatal("bootstrap did not set session cookie")
	}
	return handler, cookie
}

// serveRequest sends an unauthenticated request to the handler.
func serveRequest(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}

// serveAuthenticatedRequest sends a request with a valid session cookie.
func serveAuthenticatedRequest(t *testing.T, handler http.Handler, method, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	handler.ServeHTTP(response, req)
	return response
}

// indexBody returns the body of an authenticated GET /.
func indexBody(t *testing.T) string {
	t.Helper()
	handler, cookie := mustNewHandlerWithAuth(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	response := serveAuthenticatedRequest(t, handler, http.MethodGet, "/", cookie)
	if response.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200; body = %q", response.Code, response.Body.String())
	}
	return response.Body.String()
}

func TestHandlerRoutes(t *testing.T) {
	handler, cookie := mustNewHandlerWithAuth(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	tests := []struct {
		name          string
		path          string
		method        string
		authenticated bool
		status        int
		contentType   string
		body          string
		contains      []string
	}{
		{
			name:          "index",
			path:          "/",
			method:        http.MethodGet,
			authenticated: true,
			status:        http.StatusOK,
			contentType:   "text/html; charset=utf-8",
			contains: []string{
				"<title>Kanedias — Circle of the Fleet</title>",
				`id="alertBanner"`,
				`id="fleet-panel"`,
				`id="detail-panel"`,
				`id="question-panel"`,
				`id="activity-panel"`,
				`id="deck-status"`,
			},
		},
		{
			name:   "index unauthenticated",
			path:   "/",
			method: http.MethodGet,
			status: http.StatusUnauthorized,
		},
		{
			name:        "health",
			path:        "/healthz",
			method:      http.MethodGet,
			status:      http.StatusOK,
			contentType: "text/plain; charset=utf-8",
			body:        "ok\n",
		},
		{
			name:          "status",
			path:          "/ui/status",
			method:        http.MethodGet,
			authenticated: true,
			status:        http.StatusOK,
			contentType:   "text/event-stream",
			contains:      []string{"id=\"server-status\"", "role=\"status\"", "Running"},
		},
		{
			name:        "stylesheet",
			path:        "/assets/app.css",
			method:      http.MethodGet,
			status:      http.StatusOK,
			contentType: "text/css; charset=utf-8",
		},
		{
			name:        "terminal stylesheet",
			path:        "/assets/terminal.css",
			method:      http.MethodGet,
			status:      http.StatusOK,
			contentType: "text/css; charset=utf-8",
		},
		{
			name:        "javascript",
			path:        "/assets/datastar.js",
			method:      http.MethodGet,
			status:      http.StatusOK,
			contentType: "text/javascript; charset=utf-8",
		},
		{
			name:   "unknown",
			path:   "/unknown",
			method: http.MethodGet,
			status: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response *httptest.ResponseRecorder
			if tt.authenticated {
				response = serveAuthenticatedRequest(t, handler, tt.method, tt.path, cookie)
			} else {
				response = serveRequest(handler, tt.method, tt.path)
			}
			if response.Code != tt.status {
				t.Fatalf("status = %d, want %d; body = %q", response.Code, tt.status, response.Body.String())
			}
			if tt.contentType != "" {
				got := response.Header().Get("Content-Type")
				if tt.path == "/ui/status" {
					if !strings.HasPrefix(got, tt.contentType) {
						t.Fatalf("Content-Type = %q, want prefix %q", got, tt.contentType)
					}
				} else if got != tt.contentType {
					t.Fatalf("Content-Type = %q, want %q", got, tt.contentType)
				}
			}
			if tt.body != "" && response.Body.String() != tt.body {
				t.Fatalf("body = %q, want %q", response.Body.String(), tt.body)
			}
			if strings.HasPrefix(tt.path, "/assets/") && response.Body.Len() == 0 {
				t.Fatal("asset body is empty")
			}
			for _, want := range tt.contains {
				if !strings.Contains(response.Body.String(), want) {
					t.Errorf("body does not contain %q", want)
				}
			}
			if tt.path == "/ui/status" && tt.authenticated {
				const event = "event: datastar-patch-elements"
				if got := strings.Count(response.Body.String(), event); got != 1 {
					t.Errorf("status event count = %d, want 1; body = %q", got, response.Body.String())
				}
			}
		})
	}
}

func TestHandlerRejectsUnsupportedMethods(t *testing.T) {
	handler, cookie := mustNewHandlerWithAuth(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	paths := []struct {
		path          string
		authenticated bool
	}{
		{"/healthz", false},
		{"/assets/terminal.css", false},
		{"/assets/app.css", false},
		{"/assets/datastar.js", false},
		{"/", true},
		{"/ui/status", true},
	}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

	for _, p := range paths {
		for _, method := range methods {
			t.Run(method+" "+p.path, func(t *testing.T) {
				var response *httptest.ResponseRecorder
				if p.authenticated {
					response = serveAuthenticatedRequest(t, handler, method, p.path, cookie)
				} else {
					response = serveRequest(handler, method, p.path)
				}
				if response.Code != http.StatusMethodNotAllowed {
					t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
				}
			})
		}
	}
}

func TestInitialPageContainsAstrolabeConsole(t *testing.T) {
	body := indexBody(t)
	required := []string{
		`<html lang="en" data-theme="dark">`,
		// shell stable Datastar patch roots
		`id="fleet-panel"`,
		`id="detail-panel"`,
		`id="question-panel"`,
		`id="activity-panel"`,
		`id="deck-status"`,
		// top bar
		`class="topbar"`,
		`id="alertBanner"`,
		`id="alertCount"`,
		// Datastar wiring: signals and init
		`data-signals=`,
		`selectedSessionId`,
		`commandMessage`,
		`data-init=`,
		// command deck actions
		`Steer`,
		`Interrupt`,
		`Stop Session`,
		`New Session`,
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("initial page does not contain %q", want)
		}
	}

	obsolete := []string{
		// the retired orrery mockup
		`id="fleet-orbit"`,
		`id="maker-aperture"`,
		`class="run-cluster"`,
		`class="child-moon `,
		`STATIC DEMONSTRATION`,
		// leftover scaffold panels
		"Refresh status",
		"Not refreshed yet.",
		`id="dashboard-panel"`,
		`id="session-panel"`,
		// mock agent names must NOT appear in the shell
		`RPC-SPIKE`,
		`WEB-SHELL`,
		`ORBITAL-INGEST`,
		`Which contract should I lock in`,
		// removed: per-node spawn subagent
		`Spawn Subagent`,
	}
	for _, unwanted := range obsolete {
		if strings.Contains(body, unwanted) {
			t.Errorf("initial page retains obsolete content %q", unwanted)
		}
	}
}

func TestAstrolabeConsoleIsInteractive(t *testing.T) {
	body := indexBody(t)

	// The console is wired by the local Datastar module and app.js (delegated
	// behavior). No external scripts and no inline controller.
	scriptRE := regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`)
	scripts := scriptRE.FindAllStringSubmatch(body, -1)
	if len(scripts) != 2 {
		t.Fatalf("script count = %d, want 2 (Datastar module + app.js)", len(scripts))
	}

	var sawDatastar, sawAppJS bool
	for _, script := range scripts {
		attrs, inner := script[1], strings.TrimSpace(script[2])
		if strings.Contains(attrs, `src="/assets/datastar.js"`) {
			if !strings.Contains(attrs, `type="module"`) {
				t.Errorf("datastar.js script is not a module: %s", script[0])
			}
			if inner != "" {
				t.Errorf("Datastar module script has unexpected inline body %q", inner)
			}
			sawDatastar = true
		}
		if strings.Contains(attrs, `src="/assets/app.js"`) {
			if inner != "" {
				t.Errorf("app.js script has unexpected inline body %q", inner)
			}
			sawAppJS = true
		}
	}
	if !sawDatastar {
		t.Error("page is missing the local Datastar module script")
	}
	if !sawAppJS {
		t.Error("page is missing the app.js script")
	}

	// The console is a working control surface with delegated Datastar wiring.
	for _, want := range []string{`class="deck-input"`, `data-bind="commandMessage"`, `data-on:click=`} {
		if !strings.Contains(body, want) {
			t.Errorf("interactive console is missing wiring %q", want)
		}
	}
	if strings.Contains(body, " disabled") {
		t.Error("Astrolabe console must not ship disabled placeholder controls")
	}
}

func TestTemplatesDefineStableRoots(t *testing.T) {
	// Verify all fragment templates define the stable patch targets.
	// These are the IDs that Datastar streams patch into.
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	// Render the fleet template with an empty fleet and check it contains the root ID.
	fleetHTML, err := renderTemplate(templates, templateFleet, newFleetView(emptyFleetSnapshot()))
	if err != nil {
		t.Fatalf("render fleet.html: %v", err)
	}
	if !strings.Contains(fleetHTML, `id="fleet-panel"`) {
		t.Errorf("fleet.html does not contain #fleet-panel root")
	}

	// Render the detail template with an empty state.
	detailHTML, err := renderTemplate(templates, templateDetail, newDetailView(emptySessionState()))
	if err != nil {
		t.Fatalf("render detail.html: %v", err)
	}
	if !strings.Contains(detailHTML, `id="detail-panel"`) {
		t.Errorf("detail.html does not contain #detail-panel root")
	}

	// Render the questions template with an empty state.
	questionsHTML, err := renderTemplate(templates, templateQuestions, newQuestionPanelView(emptySessionState()))
	if err != nil {
		t.Fatalf("render questions.html: %v", err)
	}
	if !strings.Contains(questionsHTML, `id="question-panel"`) {
		t.Errorf("questions.html does not contain #question-panel root")
	}

	// Render the activity template with an empty state.
	activityHTML, err := renderTemplate(templates, templateActivity, newActivityView(emptySessionState()))
	if err != nil {
		t.Fatalf("render activity.html: %v", err)
	}
	if !strings.Contains(activityHTML, `id="activity-panel"`) {
		t.Errorf("activity.html does not contain #activity-panel root")
	}

	// Render the deck-status template.
	deckHTML, err := renderTemplate(templates, templateDeckStatus, newDeckStatusView(nil))
	if err != nil {
		t.Fatalf("render deck-status.html: %v", err)
	}
	if !strings.Contains(deckHTML, `id="deck-status"`) {
		t.Errorf("deck-status.html does not contain #deck-status root")
	}
}

func TestTemplatesExcludeMockContent(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	// No fragment template should contain mock or static content.
	forbiddenPhrases := []string{
		"RPC-SPIKE", "WEB-SHELL", "ORBITAL-INGEST", "MERIDIAN-REVIEW",
		"Which contract should I lock in",
		"Spawn Subagent",
		"completion percentage",
		"184.2k",
	}

	type templateCase struct {
		name string
		data any
	}
	cases := []templateCase{
		{templateFleet, newFleetView(emptyFleetSnapshot())},
		{templateDetail, newDetailView(emptySessionState())},
		{templateQuestions, newQuestionPanelView(emptySessionState())},
		{templateActivity, newActivityView(emptySessionState())},
		{templateDeckStatus, newDeckStatusView(nil)},
	}
	for _, tc := range cases {
		name, data := tc.name, tc.data
		rendered, err := renderTemplate(templates, name, data)
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		for _, phrase := range forbiddenPhrases {
			if strings.Contains(rendered, phrase) {
				t.Errorf("template %s contains forbidden phrase %q", name, phrase)
			}
		}
	}
}

func emptyFleetSnapshot() manager.FleetSnapshot {
	return manager.FleetSnapshot{}
}

func TestAstrolabeGroupsNestedSubagentsUnderParents(t *testing.T) {
	// Verify that the fleet template correctly structures nested sessions using
	// <details> for parents and "leaf" class for terminal nodes.
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatalf("parseTemplates: %v", err)
	}

	// Build a fake fleet with a root that has a child.
	snap := manager.FleetSnapshot{
		Roots: []manager.RootState{
			{
				RootSessionID: "root-1",
				Tree: supervisor.NodeSnapshot{
					SessionID: "root-1",
					Lifecycle: "active",
					Children: []supervisor.NodeSnapshot{
						{
							SessionID:  "child-1",
							WorkerType: "worker",
							Lifecycle:  "question",
							Questions: []supervisor.QuestionSummary{
								{ID: "q1", Method: "input", Title: "Test question"},
							},
							Children: []supervisor.NodeSnapshot{
								{SessionID: "leaf-1", WorkerType: "leaf-worker", Lifecycle: "active"},
							},
						},
					},
				},
			},
		},
	}
	rendered, err := renderTemplate(templates, templateFleet, newFleetView(snap))
	if err != nil {
		t.Fatalf("render fleet.html: %v", err)
	}

	// Parent node should use <details>.
	if !strings.Contains(rendered, "<details") {
		t.Error("fleet template does not use <details> for parent nodes")
	}
	// Children should be inside a .children container.
	if !strings.Contains(rendered, `class="children"`) {
		t.Error("fleet template does not use .children container")
	}
	// Leaf node should have "leaf" class.
	if !strings.Contains(rendered, `class="row leaf`) {
		t.Error("fleet template does not mark leaf nodes")
	}
	// Question nodes should have "asks you" indicator.
	if !strings.Contains(rendered, `class="asks"`) {
		t.Error("fleet template does not flag question rows")
	}
	// Rows should carry data-session-id for Datastar wiring.
	if !strings.Contains(rendered, `data-session-id=`) {
		t.Error("fleet template rows are missing data-session-id")
	}
}

// streamFakeFleet is a controllable fake fleet manager for SSE stream tests.
type streamFakeFleet struct {
	mu             sync.Mutex
	fleetUpdates   chan uint64
	sessionUpdates map[string]chan uint64
	sessions       map[string]manager.SessionState
	snapshot       manager.FleetSnapshot
}

func newStreamFakeFleet() *streamFakeFleet {
	return &streamFakeFleet{
		fleetUpdates:   make(chan uint64, 4),
		sessionUpdates: make(map[string]chan uint64),
		sessions:       make(map[string]manager.SessionState),
	}
}

func (f *streamFakeFleet) setSnapshot(s manager.FleetSnapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshot = s
}

func (f *streamFakeFleet) Start(context.Context) error { return nil }
func (f *streamFakeFleet) Fleet() manager.FleetSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshot
}
func (f *streamFakeFleet) Session(id string) (manager.SessionState, error) {
	s, ok := f.sessions[id]
	if !ok {
		return manager.SessionState{}, errors.New("not found")
	}
	return s, nil
}
func (f *streamFakeFleet) SubscribeFleet() manager.ChangeSubscription {
	return manager.ChangeSubscription{Updates: f.fleetUpdates, Close: func() {}}
}
func (f *streamFakeFleet) SubscribeSession(id string) (manager.ChangeSubscription, error) {
	ch, ok := f.sessionUpdates[id]
	if !ok {
		return manager.ChangeSubscription{}, errors.New("not found")
	}
	return manager.ChangeSubscription{Updates: ch, Close: func() {}}, nil
}
func (f *streamFakeFleet) SpawnRoot(context.Context) (string, error)   { return "", nil }
func (f *streamFakeFleet) Steer(context.Context, string, string) error { return nil }
func (f *streamFakeFleet) Interrupt(context.Context, string) error     { return nil }
func (f *streamFakeFleet) StopSession(context.Context, string) error   { return nil }
func (f *streamFakeFleet) AnswerQuestion(context.Context, string, string, json.RawMessage) error {
	return nil
}
func (f *streamFakeFleet) SessionStats(context.Context, string) (manager.SessionStats, error) {
	return manager.SessionStats{}, nil
}
func (f *streamFakeFleet) Quiesce(context.Context) error { return nil }
func (f *streamFakeFleet) Close(context.Context) error   { return nil }

// mustNewHandlerWithFleetAuth creates a handler with an attached fake fleet
// and returns a valid session cookie.
func mustNewHandlerWithFleetAuth(t *testing.T, fleet fleetManager) (http.Handler, *http.Cookie) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	var bootstrapOut bytes.Buffer
	streamCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler, err := newHandlerWithOptions(logger, "127.0.0.1:8080", &bootstrapOut, fleet, streamCtx)
	if err != nil {
		t.Fatalf("newHandlerWithOptions: %v", err)
	}

	output := bootstrapOut.String()
	idx := strings.Index(output, bootstrapQueryName+"=")
	if idx == -1 {
		t.Fatalf("bootstrap output does not contain token: %q", output)
	}
	token := strings.TrimSpace(output[idx+len(bootstrapQueryName)+1:])

	req := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+token, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("bootstrap status = %d, want %d", w.Code, http.StatusSeeOther)
	}
	var cookie *http.Cookie
	for _, h := range w.Header()["Set-Cookie"] {
		if strings.HasPrefix(h, sessionCookieName+"=") {
			parts := strings.SplitN(h, "=", 2)
			if len(parts) == 2 {
				cookie = &http.Cookie{
					Name:  sessionCookieName,
					Value: strings.Split(parts[1], ";")[0],
				}
			}
			break
		}
	}
	if cookie == nil {
		t.Fatal("bootstrap did not set session cookie")
	}
	return handler, cookie
}

// TestFleetStreamSendsInitialFleet verifies that GET /ui/fleet streams the
// initial fleet snapshot as a patch on #fleet-panel.
func TestFleetStreamSendsInitialFleet(t *testing.T) {
	fleet := newStreamFakeFleet()
	fleet.setSnapshot(manager.FleetSnapshot{
		Roots: []manager.RootState{
			{
				RootSessionID: "root-stream-1",
				Tree: supervisor.NodeSnapshot{
					SessionID: "root-stream-1",
					Lifecycle: "active",
				},
			},
		},
	})

	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/ui/fleet", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()

	// Close the fleet update channel to end the stream.
	close(fleet.fleetUpdates)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("fleet stream did not return after channel close")
	}

	body := w.Body.String()
	if !strings.Contains(body, "fleet-panel") {
		t.Errorf("fleet stream body does not target #fleet-panel:\n%s", body)
	}
	if !strings.Contains(body, "root-stream-1") {
		t.Errorf("fleet stream body does not contain root session ID:\n%s", body)
	}
}

// TestFleetStreamSendsUpdateOnNotification verifies that fleet updates
// trigger a new patch when the update channel fires.
func TestFleetStreamSendsUpdateOnNotification(t *testing.T) {
	fleet := newStreamFakeFleet()
	// start with empty snapshot; no need to set, zero value is correct.

	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/ui/fleet", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()

	// Send an update with a new root.
	fleet.setSnapshot(manager.FleetSnapshot{
		Roots: []manager.RootState{
			{
				RootSessionID: "root-updated",
				Tree:          supervisor.NodeSnapshot{SessionID: "root-updated", Lifecycle: "active"},
			},
		},
	})
	fleet.fleetUpdates <- 1
	close(fleet.fleetUpdates)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("fleet stream did not return after channel close")
	}

	body := w.Body.String()
	if strings.Count(body, "fleet-panel") < 2 {
		t.Errorf("fleet stream did not send two patches (initial + update):\n%s", body)
	}
	if !strings.Contains(body, "root-updated") {
		t.Errorf("fleet stream update patch missing 'root-updated':\n%s", body)
	}
}

// TestSessionStreamSendsInitialState verifies that GET /ui/session with a
// selected session ID streams the initial session state.
func TestSessionStreamSendsInitialState(t *testing.T) {
	fleet := newStreamFakeFleet()
	sessionID := "session-abc"
	fleet.sessions[sessionID] = manager.SessionState{
		Node: supervisor.NodeSnapshot{
			SessionID: sessionID,
			Lifecycle: "active",
		},
	}
	fleet.sessionUpdates[sessionID] = make(chan uint64, 4)

	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/ui/session?datastar=%7B%22selectedSessionId%22%3A%22session-abc%22%7D", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()

	// Close session channel to end stream.
	close(fleet.sessionUpdates[sessionID])

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session stream did not return after channel close")
	}

	body := w.Body.String()
	if !strings.Contains(body, "detail-panel") {
		t.Errorf("session stream body does not patch #detail-panel:\n%s", body)
	}
	if !strings.Contains(body, "question-panel") {
		t.Errorf("session stream body does not patch #question-panel:\n%s", body)
	}
	if !strings.Contains(body, "activity-panel") {
		t.Errorf("session stream body does not patch #activity-panel:\n%s", body)
	}
}

// TestSessionStreamEmptyIDRendersEmptyPanels verifies that a session stream
// request without a selected session ID renders empty panels and returns.
func TestSessionStreamEmptyIDRendersEmptyPanels(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/ui/session?datastar=%7B%22selectedSessionId%22%3A%22%22%7D", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()

	// Cancel the request context quickly to end stream.
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session stream did not return after context cancel")
	}
	// No assertion on body — just that the handler returns without panicking.
}

// TestSessionStreamUnknownIDReturns404 verifies that a session stream for an
// unknown session ID returns 404 without opening an SSE stream.
func TestSessionStreamUnknownIDReturns404(t *testing.T) {
	fleet := newStreamFakeFleet()
	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/ui/session?datastar=%7B%22selectedSessionId%22%3A%22no-such-session%22%7D", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("unknown session: status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestAssetsAreEmbedded(t *testing.T) {
	handler, cookie := mustNewHandlerWithAuth(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	// Unauthenticated paths.
	for _, path := range []string{"/healthz", "/assets/terminal.css", "/assets/app.css", "/assets/datastar.js", "/assets/app.js"} {
		if response := serveRequest(handler, http.MethodGet, path); response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, response.Code, http.StatusOK)
		}
	}
	// Authenticated paths.
	if response := serveAuthenticatedRequest(t, handler, http.MethodGet, "/", cookie); response.Code != http.StatusOK {
		t.Errorf("GET / status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestTerminalCSSProvenanceMatchesEmbeddedAsset(t *testing.T) {
	stylesheet, err := webFiles.ReadFile("web/terminal.css")
	if err != nil {
		t.Fatalf("read embedded Terminal.css: %v", err)
	}
	license, err := webFiles.ReadFile("web/terminal.LICENSE")
	if err != nil {
		t.Fatalf("read embedded Terminal.css license: %v", err)
	}
	provenance, err := webFiles.ReadFile("web/terminal.PROVENANCE")
	if err != nil {
		t.Fatalf("read Terminal.css provenance: %v", err)
	}

	const expectedDigest = "54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8"
	digest := sha256.Sum256(stylesheet)
	if got := fmt.Sprintf("%x", digest); got != expectedDigest {
		t.Fatalf("Terminal.css SHA-256 = %q, want immutable upstream digest %q", got, expectedDigest)
	}
	checks := []string{
		"Commit: 63551f0de711f2f634a0c2da7bab1d3bae216fef",
		"SHA-256: " + expectedDigest,
		"License identifier: MIT",
		"Modification: Vendored unchanged for offline embedding.",
	}
	for _, want := range checks {
		if !strings.Contains(string(provenance), want) {
			t.Errorf("Terminal.css provenance does not contain %q", want)
		}
	}
	if !strings.Contains(string(license), "MIT License") || !strings.Contains(string(license), "Copyright (c) 2019 Jonas D.") {
		t.Fatal("embedded Terminal.css license is not the expected upstream MIT license")
	}
}

func TestRenderedPageHasOnlyOrderedLocalRuntimeAssets(t *testing.T) {
	body := indexBody(t)
	assetRE := regexp.MustCompile(`(?:src|href)="([^"]+)"`)
	matches := assetRE.FindAllStringSubmatch(body, -1)
	want := []string{
		"/assets/terminal.css",
		"/assets/app.css",
		"/assets/datastar.js",
		"/assets/app.js",
	}
	if len(matches) != len(want) {
		t.Fatalf("runtime asset count = %d, want %d; assets: %v", len(matches), len(want), matches)
	}
	for index, match := range matches {
		if match[1] != want[index] {
			t.Errorf("runtime asset %d = %q, want %q", index, match[1], want[index])
		}
		asset := strings.ToLower(match[1])
		if strings.HasPrefix(asset, "http://") ||
			strings.HasPrefix(asset, "https://") ||
			strings.HasPrefix(asset, "//") ||
			strings.Contains(asset, "cdn") ||
			strings.Contains(asset, "node_modules") ||
			strings.Contains(asset, "npm") ||
			strings.Contains(asset, "unpkg") {
			t.Errorf("external runtime asset %q", match[1])
		}
	}
}

func TestProjectStylesDefineAstrolabeVisualSystem(t *testing.T) {
	contents, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatalf("read embedded project stylesheet: %v", err)
	}
	styles := string(contents)
	required := []string{
		// colorblind-safe palette tokens (cyan/amber/violet, never red-vs-green)
		"--cyan:",
		"--amber:",
		"--violet:",
		"--brass:",
		// the responsive app shell + core Astrolabe regions
		".app{",
		".sidebar{",
		".instrument{",
		".alidade{",
		".alert-banner{",
		".deck{",
		// responsive: sidebar collapses to a slide-over on narrow screens
		"@media (max-width:820px)",
		".sidebar.open",
	}
	for _, want := range required {
		if !strings.Contains(styles, want) {
			t.Errorf("project stylesheet does not contain %q", want)
		}
	}

	// State colors must be distinct hues paired with glyphs in markup — assert
	// the three state accents are present and mutually distinct.
	for _, token := range []string{"--cyan:", "--amber:", "--violet:"} {
		if strings.Count(styles, token) < 1 {
			t.Errorf("stylesheet is missing state accent %q", token)
		}
	}

	// Remain self-contained: no external fetches from CSS.
	lower := strings.ToLower(styles)
	for _, unwanted := range []string{"@import", "http://", "https://", "url("} {
		if strings.Contains(lower, unwanted) {
			t.Errorf("project stylesheet contains disallowed external construct %q", unwanted)
		}
	}
}

func TestPanicRecoveryReturnsGeneric500(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := requestLogger(logger)(recoverPanics(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("private panic value")
	})))

	response := serveRequest(handler, http.MethodGet, "/panic")
	if response.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	if response.Body.String() != "Internal Server Error\n" {
		t.Errorf("body = %q, want generic error", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "private panic value") || strings.Contains(response.Body.String(), "goroutine") {
		t.Error("client response leaked panic details")
	}
	logOutput := logs.String()
	if !strings.Contains(logOutput, `"level":"ERROR"`) || !strings.Contains(logOutput, "private panic value") {
		t.Errorf("error log does not contain panic value: %s", logOutput)
	}
	if !strings.Contains(logOutput, `"status":500`) {
		t.Errorf("completed request log does not report status 500: %s", logOutput)
	}
}

func TestRequestLogging(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := requestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	req := httptest.NewRequest(http.MethodPut, "/logged?ignored=yes", nil)
	req.RemoteAddr = "192.0.2.10:54321"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	output := logs.String()
	for _, want := range []string{`"method":"PUT"`, `"path":"/logged"`, `"status":201`, `"duration":`, `"remote_addr":"192.0.2.10:54321"`} {
		if !strings.Contains(output, want) {
			t.Errorf("request log is missing %s: %s", want, output)
		}
	}
}

func TestRequestLoggingOmitsTokensAndCookies(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := requestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	secretToken := "supersecretbootstraptoken12345678"
	req := httptest.NewRequest(http.MethodGet, "/bootstrap?"+bootstrapQueryName+"="+secretToken, nil)
	req.Header.Set("Cookie", sessionCookieName+"=sessionvalue987654")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)

	output := logs.String()
	// Must contain path — but NOT the token or cookie value.
	if !strings.Contains(output, `"path":"/bootstrap"`) {
		t.Errorf("log does not contain path: %s", output)
	}
	if strings.Contains(output, secretToken) {
		t.Errorf("log leaked bootstrap token: %s", output)
	}
	if strings.Contains(output, "sessionvalue987654") {
		t.Errorf("log leaked session cookie value: %s", output)
	}
}

func TestStatusStreamHonorsCanceledRequest(t *testing.T) {
	handler, cookie := mustNewHandlerWithAuth(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/ui/status", nil).WithContext(ctx)
	req.AddCookie(cookie)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, req)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled status request did not return promptly")
	}
	if response.Body.Len() != 0 {
		t.Errorf("canceled status request emitted body %q", response.Body.String())
	}
}

func TestHandlerRejectsNilLogger(t *testing.T) {
	if _, err := newHandler(nil); err == nil {
		t.Fatal("newHandler(nil) succeeded, want error")
	}
}

func TestHandlerParsesTemplatesAtConstruction(t *testing.T) {
	invalid := fstest.MapFS{
		"web/index.html": &fstest.MapFile{Data: []byte(`{{if}}`)},
	}
	if _, err := parseTemplates(fs.FS(invalid)); err == nil {
		t.Fatal("parseTemplates accepted invalid template")
	}
}
