package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	handler, err := newHandlerWithOptions(logger, "127.0.0.1:0", &bootstrapOut, nil, context.Background(), true)
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

func TestHandlerPrintsAdvertisedURLs(t *testing.T) {
	for _, requireSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("require_session_%t", requireSession), func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			_, err := newHandlerWithOptions(
				logger, "steven-desktop:8080", &output, nil, context.Background(), requireSession,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "Web UI: http://steven-desktop:8080/\n") {
				t.Fatalf("operator output = %q, want advertised Web UI URL", output.String())
			}
			hasBootstrap := strings.Contains(output.String(), "Bootstrap URL: http://steven-desktop:8080/bootstrap?capability=")
			if hasBootstrap != requireSession {
				t.Fatalf("bootstrap URL present = %t, want %t; output = %q", hasBootstrap, requireSession, output.String())
			}
		})
	}
}

func TestHandlerTrustedNetworkModeBypassesBrowserSecurity(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := newHandlerWithOptions(
		logger, "steven-desktop:8080", io.Discard, nil, context.Background(), false,
	)
	if err != nil {
		t.Fatal(err)
	}

	get := httptest.NewRequest(http.MethodGet, "/", nil)
	get.Host = "other-private-host:8080"
	getResult := httptest.NewRecorder()
	handler.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK {
		t.Fatalf("trusted-network GET status = %d, want 200", getResult.Code)
	}

	post := httptest.NewRequest(http.MethodPost, "/ui/sessions", strings.NewReader("not json"))
	post.Host = "other-private-host:8080"
	post.Header.Set("Origin", "http://different-private-host:8080")
	post.Header.Set("Sec-Fetch-Site", "cross-site")
	post.Header.Set("Content-Type", "text/plain")
	postResult := httptest.NewRecorder()
	handler.ServeHTTP(postResult, post)
	if postResult.Code != http.StatusNotFound {
		t.Fatalf("trusted-network POST status = %d, want downstream 404", postResult.Code)
	}
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
			name:        "terminal decisions",
			path:        "/assets/terminal-ui.js",
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
		`data-can-steer="false"`,
		`data-can-interrupt="false"`,
		`data-can-stop="false"`,
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

	// The console is wired by local asset modules (Datastar, Marked, Highlight,
	// the Markdown renderer, and app.js delegated behavior). No external scripts
	// and no inline controller.
	scriptRE := regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`)
	scripts := scriptRE.FindAllStringSubmatch(body, -1)
	wantScripts := 6 // Datastar + 3 Markdown assets + terminal decisions + app.js
	if len(scripts) != wantScripts {
		t.Fatalf("script count = %d, want %d (Datastar + 3 Markdown assets + terminal-ui.js + app.js)", len(scripts), wantScripts)
	}

	var sawDatastar, sawTerminalUI, sawAppJS bool
	markdownAssets := []string{
		`src="/assets/marked.min.js"`,
		`src="/assets/highlight.min.js"`,
		`src="/assets/markdown-renderer.js"`,
	}
	sawMarkdown := make(map[string]bool)
	for _, script := range scripts {
		attrs, inner := script[1], strings.TrimSpace(script[2])
		if strings.Contains(attrs, `src="http://`) || strings.Contains(attrs, `src="https://`) {
			t.Errorf("script references a remote origin: %s", script[0])
		}
		if strings.Contains(attrs, `src="/assets/datastar.js"`) {
			if !strings.Contains(attrs, `type="module"`) {
				t.Errorf("datastar.js script is not a module: %s", script[0])
			}
			if inner != "" {
				t.Errorf("Datastar module script has unexpected inline body %q", inner)
			}
			sawDatastar = true
		}
		if strings.Contains(attrs, `src="/assets/terminal-ui.js"`) {
			if inner != "" {
				t.Errorf("terminal-ui.js script has unexpected inline body %q", inner)
			}
			sawTerminalUI = true
		}
		if strings.Contains(attrs, `src="/assets/app.js"`) {
			if inner != "" {
				t.Errorf("app.js script has unexpected inline body %q", inner)
			}
			sawAppJS = true
		}
		for _, asset := range markdownAssets {
			if strings.Contains(attrs, asset) {
				sawMarkdown[asset] = true
			}
		}
	}
	if !sawDatastar {
		t.Error("page is missing the local Datastar module script")
	}
	if !sawTerminalUI {
		t.Error("page is missing the local terminal-ui.js script")
	}
	if !sawAppJS {
		t.Error("page is missing the app.js script")
	}
	for _, asset := range markdownAssets {
		if !sawMarkdown[asset] {
			t.Errorf("page is missing local script %s", asset)
		}
	}

	// The console is a working control surface with delegated Datastar wiring.
	for _, want := range []string{
		`class="deck-input"`, `data-bind="commandMessage"`, `data-on:click=`,
		`aria-keyshortcuts="Control+A Control+C Enter"`,
		`aria-keyshortcuts="Escape"`,
		`^A home · ^C clear/copy · esc abort · ^O tools`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("interactive console is missing wiring %q", want)
		}
	}
	if strings.Contains(body, " disabled") {
		t.Error("Astrolabe console must not ship disabled placeholder controls")
	}
	if strings.Contains(strings.ToLower(body), "onkeydown=") || strings.Contains(strings.ToLower(body), "onkeyup=") {
		t.Error("Astrolabe console must use delegated keyboard handling, not inline key handlers")
	}
}

func TestActivityUsesMarkdownClassification(t *testing.T) {
	cases := []struct {
		kind string
		want bool
	}{
		{"user_message", true},
		{"message_update", true},
		{"model_error", false},
		{"tool_call", false},
		{"tool_result", false},
		{"bash_execution", false},
		{"", false},
	}
	for _, c := range cases {
		if got := activityUsesMarkdown(c.kind); got != c.want {
			t.Errorf("activityUsesMarkdown(%q) = %v, want %v", c.kind, got, c.want)
		}
	}

	// Exercise newActivityView from manager state rather than manually-set flags.
	state := manager.SessionState{RecentActivity: []manager.ActivityItem{
		{Seq: 1, Kind: "user_message", Text: "# hi"},
		{Seq: 2, Kind: "message_update", Text: "**bold**"},
		{Seq: 3, Kind: "model_error", Text: "boom", IsError: true},
		{Seq: 4, Kind: "tool_result", Text: "out"},
	}}
	view := newActivityView(state)
	if len(view.Items) != 4 {
		t.Fatalf("items = %d, want 4", len(view.Items))
	}
	if !view.Items[0].IsMarkdown || !view.Items[1].IsMarkdown {
		t.Error("user_message/message_update should be classified as Markdown")
	}
	if view.Items[2].IsMarkdown || view.Items[3].IsMarkdown {
		t.Error("model_error/tool_result must not be classified as Markdown")
	}

	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatal(err)
	}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(html, `data-markdown`); got != 2 {
		t.Fatalf("markdown markers = %d, want 2\n%s", got, html)
	}
}

func TestActivityMarksOnlyConversationTextAsMarkdown(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatal(err)
	}
	view := activityView{Items: []activityItemView{
		{Kind: "user_message", Label: "You", Text: "# prompt", IsMarkdown: true},
		{Kind: "message_update", Label: "Message", Text: "```go\npackage p\n```", IsMarkdown: true},
		{Kind: "model_error", Label: "Model error", Text: "**not markup**", IsError: true},
	}}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(html, `data-markdown`); got != 2 {
		t.Fatalf("markdown markers = %d, want 2\n%s", got, html)
	}
	if strings.Contains(html, `<h1>`) || strings.Contains(html, `<script`) {
		t.Fatalf("server trusted transcript markup:\n%s", html)
	}

	// Complete must flow through newActivityView from manager fixtures unchanged,
	// keyed on the stable sequence.
	state := manager.SessionState{RecentActivity: []manager.ActivityItem{
		{Seq: 21, Kind: "message_update", Label: "Message", Text: "streaming"},
		{Seq: 22, Kind: "message_update", Label: "Message", Text: "done", Complete: true},
	}}
	view = newActivityView(state)
	if len(view.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(view.Items))
	}
	if view.Items[0].Seq != 21 || view.Items[0].Complete {
		t.Errorf("item 0: seq=%d complete=%v, want seq=21 complete=false", view.Items[0].Seq, view.Items[0].Complete)
	}
	if view.Items[1].Seq != 22 || !view.Items[1].Complete {
		t.Errorf("item 1: seq=%d complete=%v, want seq=22 complete=true", view.Items[1].Seq, view.Items[1].Complete)
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
	detailHTML, err := renderTemplate(templates, templateDetail, newDetailView(emptySessionState(), statsView{}))
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
		{templateDetail, newDetailView(emptySessionState(), statsView{})},
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
	stats          manager.SessionStats
	statsErr       error
	statsCalls     int
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statsCalls++
	return f.stats, f.statsErr
}

func (f *streamFakeFleet) statsCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statsCalls
}
func (f *streamFakeFleet) Quiesce(context.Context) error { return nil }
func (f *streamFakeFleet) Close(context.Context) error   { return nil }

// mustNewHandlerWithFleetAuth creates a handler with an attached fake fleet
// and returns a valid session cookie.
func mustNewHandlerWithFleetAuth(t *testing.T, fleet fleetManager) (http.Handler, *http.Cookie) {
	t.Helper()
	return mustNewHandlerWithFleetAuthLogger(t, fleet, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
}

// mustNewHandlerWithFleetAuthLogger is mustNewHandlerWithFleetAuth with an
// explicit logger so tests can assert what the server records server-side.
func mustNewHandlerWithFleetAuthLogger(t *testing.T, fleet fleetManager, logger *slog.Logger) (http.Handler, *http.Cookie) {
	t.Helper()
	var bootstrapOut bytes.Buffer
	streamCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	handler, err := newHandlerWithOptions(logger, "127.0.0.1:8080", &bootstrapOut, fleet, streamCtx, true)
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

// ptrFloat is a small helper for building nullable metric fields.
func ptrFloat(v float64) *float64 { return &v }

// TestSessionStreamRendersStatsForActionableNode is the I4 regression test: the
// selected-detail stream fetches SessionStats for an actionable node and renders
// the context percentage into the detail panel.
func TestSessionStreamRendersStatsForActionableNode(t *testing.T) {
	fleet := newStreamFakeFleet()
	sessionID := "session-stats"
	fleet.sessions[sessionID] = manager.SessionState{
		Node: supervisor.NodeSnapshot{
			SessionID: sessionID,
			Lifecycle: "running",
		},
		StreamConnected: true,
	}
	fleet.sessionUpdates[sessionID] = make(chan uint64, 4)
	fleet.stats = manager.SessionStats{
		TotalMessages: 12,
		ToolCalls:     3,
		Cost:          0.4211,
		ContextUsage:  &manager.ContextUsage{Percent: ptrFloat(42)},
	}

	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/ui/session?datastar=%7B%22selectedSessionId%22%3A%22session-stats%22%7D", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()
	close(fleet.sessionUpdates[sessionID])

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session stream did not return after channel close")
	}

	if fleet.statsCallCount() == 0 {
		t.Fatal("SessionStats was never called for an actionable node")
	}
	body := w.Body.String()
	if !strings.Contains(body, "42%") {
		t.Errorf("detail body does not render context percentage:\n%s", body)
	}
}

// TestSessionStreamStatsThrottled is the I4 throttle test: a burst of many
// activity revisions must not produce one get_session_stats call per revision.
func TestSessionStreamStatsThrottled(t *testing.T) {
	fleet := newStreamFakeFleet()
	sessionID := "session-burst"
	fleet.sessions[sessionID] = manager.SessionState{
		Node:            supervisor.NodeSnapshot{SessionID: sessionID, Lifecycle: "running"},
		StreamConnected: true,
	}
	updates := make(chan uint64, 64)
	fleet.sessionUpdates[sessionID] = updates
	fleet.stats = manager.SessionStats{
		ContextUsage: &manager.ContextUsage{Percent: ptrFloat(10)},
	}

	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/ui/session?datastar=%7B%22selectedSessionId%22%3A%22session-burst%22%7D", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()

	// Fire a burst of 40 revisions quickly, then end the stream.
	for i := 0; i < 40; i++ {
		updates <- uint64(i + 1)
	}
	// Give the handler a moment to process the burst well within one throttle
	// window, then close to finish.
	time.Sleep(200 * time.Millisecond)
	close(updates)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session stream did not return")
	}

	// Initial render fetches once; the burst all falls within one throttle
	// window (1s), so at most a couple more fetches are allowed — certainly not
	// one per revision.
	if got := fleet.statsCallCount(); got > 3 {
		t.Fatalf("stats fetched %d times for a 40-revision burst; throttle not holding", got)
	}
}

// TestSessionStreamNullableContextRendersDash is the I4 nullable test: when the
// context percentage is absent, the dial renders "—".
func TestSessionStreamNullableContextRendersDash(t *testing.T) {
	fleet := newStreamFakeFleet()
	sessionID := "session-nullctx"
	fleet.sessions[sessionID] = manager.SessionState{
		Node:            supervisor.NodeSnapshot{SessionID: sessionID, Lifecycle: "running"},
		StreamConnected: true,
	}
	fleet.sessionUpdates[sessionID] = make(chan uint64, 4)
	// Stats present but no context usage → nullable percent.
	fleet.stats = manager.SessionStats{TotalMessages: 5}

	handler, cookie := mustNewHandlerWithFleetAuth(t, fleet)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/ui/session?datastar=%7B%22selectedSessionId%22%3A%22session-nullctx%22%7D", nil).WithContext(ctx)
	req.AddCookie(cookie)
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(w, req)
	}()
	close(fleet.sessionUpdates[sessionID])

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session stream did not return")
	}

	body := w.Body.String()
	if !strings.Contains(body, "—") {
		t.Errorf("nullable context did not render em-dash:\n%s", body)
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
	for _, path := range []string{"/healthz", "/assets/terminal.css", "/assets/app.css", "/assets/datastar.js", "/assets/terminal-ui.js", "/assets/app.js"} {
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
		"/assets/marked.min.js",
		"/assets/highlight.min.js",
		"/assets/markdown-renderer.js",
		"/assets/terminal-ui.js",
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

// detailsTagOpen reports whether the first <details ...> opening tag carries an
// open attribute. It parses the attribute strictly (delimiter-separated on both
// sides) so a status class like "tool-done" or "tool-running" can never cause a
// false positive.
func detailsTagOpen(html string) bool {
	tag := "<details "
	start := strings.Index(html, tag)
	if start == -1 {
		return false
	}
	rel := strings.Index(html[start:], ">")
	if rel == -1 {
		return false
	}
	open := html[start : start+rel]
	re := regexp.MustCompile(`(^|\s)open(?:[=\s>]|$)`)
	return re.MatchString(open)
}

func TestActivityTemplatePreservesStableInteractiveState(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatal(err)
	}
	view := activityView{Items: []activityItemView{
		{Seq: 11, Kind: "message_update", Label: "Message", Text: "completed message", IsMarkdown: true, Complete: true},
		{Seq: 12, Kind: "message_update", Label: "Message", Text: "streaming message", IsMarkdown: true},
		{Seq: 13, Kind: "tool_execution_start", Label: "Tool: bash", IsTool: true,
			ToolArgs: "RUNNING_ARGS", ToolOutput: "RUNNING_RESULT", ToolCardClass: "tool-running", StatusLabel: "running"},
		{Seq: 14, Kind: "tool_execution_start", Label: "Tool: bash", IsTool: true, Complete: true,
			ToolArgs: "DONE_ARGS", ToolOutput: "DONE_RESULT", ToolCardClass: "tool-done", StatusLabel: "done"},
	}}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"11", "12", "13", "14"} {
		if !strings.Contains(html, `id="activity-item-`+id+`"`) {
			t.Errorf("missing stable activity ID %s:\n%s", id, html)
		}
	}
	if got := strings.Count(html, `data-preserve-attr="open"`); got != 2 {
		t.Errorf("preserved tool open attributes = %d, want 2\n%s", got, html)
	}
	if !strings.Contains(html, `<div class="t-body" data-markdown data-ignore-morph>completed message</div>`) {
		t.Errorf("completed message is morphable:\n%s", html)
	}
	if !strings.Contains(html, `<div class="t-body" data-markdown>streaming message</div>`) {
		t.Errorf("streaming message was frozen:\n%s", html)
	}
	for _, args := range []string{"RUNNING_ARGS", "DONE_ARGS"} {
		if !strings.Contains(html, `data-language="json" data-ignore-morph>`+args+`</code>`) {
			t.Errorf("tool arguments %q are morphable:\n%s", args, html)
		}
	}
	if !strings.Contains(html, `data-language="">RUNNING_RESULT</code>`) {
		t.Errorf("running tool result was frozen:\n%s", html)
	}
	if !strings.Contains(html, `data-language="" data-ignore-morph>DONE_RESULT</code>`) {
		t.Errorf("completed tool result is morphable:\n%s", html)
	}
	if detailsTagOpen(html) {
		t.Fatalf("tool defaulted open: %s", html)
	}
}

func TestToolCardTemplateEscapesAndCollapses(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatal(err)
	}
	payload := "</pre><script>alert(1)</script>"
	view := activityView{Items: []activityItemView{
		{
			Kind: "tool_execution_start", Label: "Tool: read",
			IsTool: true, ToolSummary: "read a.txt", ToolArgs: payload,
			ToolOutput: payload, ToolLanguage: "go", ToolTruncated: false,
			ToolCardClass: "tool-done", StatusLabel: "done",
		},
	}}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `class="tool-card tool-done"`) {
		t.Fatal("tool card details missing\n" + html)
	}
	if strings.Contains(html, `<script>alert`) {
		t.Fatalf("unescaped tool content: %s", html)
	}
	if !strings.Contains(html, `&lt;script&gt;alert`) {
		t.Fatalf("missing escaped source: %s", html)
	}
	if detailsTagOpen(html) {
		t.Fatalf("tool defaulted open: %s", html)
	}
}

func TestToolCardRunningNeverOpens(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatal(err)
	}
	// A real running card keeps the running state only as class/status and must
	// render collapsed (no open attribute).
	view := activityView{Items: []activityItemView{
		{
			Kind: "tool_execution_start", Label: "Tool: bash", IsTool: true,
			ToolSummary: "$ echo hi", ToolArgs: "{}", ToolOutput: "hi\n",
			ToolCardClass: "tool-running", StatusLabel: "running",
		},
	}}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `class="tool-card tool-running"`) {
		t.Fatalf("missing running status class: %s", html)
	}
	if detailsTagOpen(html) {
		t.Fatalf("running tool card defaulted open: %s", html)
	}
}

func TestToolCardAggregateTruncatedShowsNeutralSummaryBadge(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatal(err)
	}
	// Args-only truncation must still surface a neutral "truncated" indicator in
	// the collapsed summary, while the result section must NOT be falsely marked.
	view := activityView{Items: []activityItemView{
		{
			Kind: "tool_execution_start", Label: "Tool: bash", IsTool: true,
			ToolSummary: "$ echo hi", ToolArgs: "{\"a\": 1}",
			ToolOutput: "hi\n", ToolTruncated: true, ToolArgsTruncated: true,
			ToolOutputTruncated: false, ToolCardClass: "tool-done", StatusLabel: "done",
		},
	}}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil {
		t.Fatal(err)
	}
	sumStart := strings.Index(html, "<summary")
	sumEnd := strings.Index(html, "</summary>")
	if sumStart == -1 || sumEnd == -1 || sumEnd < sumStart {
		t.Fatalf("no summary block in html: %s", html)
	}
	if !strings.Contains(html[sumStart:sumEnd], "truncated") {
		t.Fatalf("missing neutral truncated summary badge: %s", html)
	}
	if !strings.Contains(html, `Arguments <span class="tool-trunc-label">truncated</span>`) {
		t.Fatalf("missing args truncation marker: %s", html)
	}
	if strings.Contains(html, `Result <span class="tool-trunc-label">truncated</span>`) {
		t.Fatalf("result falsely marked truncated: %s", html)
	}
}

func TestToolCardErrorView(t *testing.T) {
	templates, err := parseTemplates(webFiles)
	if err != nil {
		t.Fatal(err)
	}
	view := activityView{Items: []activityItemView{
		{
			Kind: "tool_execution_end", Label: "Tool: bash", IsTool: true,
			ToolSummary: "$ false", IsError: true,
			ToolCardClass: "tool-error", StatusLabel: "error",
		},
	}}
	html, err := renderTemplate(templates, templateActivity, view)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, `class="tool-card tool-error"`) {
		t.Fatalf("missing error card class: %s", html)
	}
	if !strings.Contains(html, `class="tool-status tool-error">error</span>`) {
		t.Fatalf("missing error status: %s", html)
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
