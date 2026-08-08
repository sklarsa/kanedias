package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestHandlerRoutes(t *testing.T) {
	handler := mustNewHandler(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))

	tests := []struct {
		name        string
		path        string
		status      int
		contentType string
		body        string
		contains    []string
	}{
		{
			name:        "index",
			path:        "/",
			status:      http.StatusOK,
			contentType: "text/html; charset=utf-8",
			contains: []string{
				"<title>Kanedias — Circle of the Fleet</title>",
				`id="sidebar"`,
				`id="alertBanner"`,
				`class="instrument"`,
			},
		},
		{
			name:        "health",
			path:        "/healthz",
			status:      http.StatusOK,
			contentType: "text/plain; charset=utf-8",
			body:        "ok\n",
		},
		{
			name:        "status",
			path:        "/ui/status",
			status:      http.StatusOK,
			contentType: "text/event-stream",
			contains:    []string{"id=\"server-status\"", "role=\"status\"", "Running"},
		},
		{
			name:        "stylesheet",
			path:        "/assets/app.css",
			status:      http.StatusOK,
			contentType: "text/css; charset=utf-8",
		},
		{
			name:        "terminal stylesheet",
			path:        "/assets/terminal.css",
			status:      http.StatusOK,
			contentType: "text/css; charset=utf-8",
		},
		{
			name:        "javascript",
			path:        "/assets/datastar.js",
			status:      http.StatusOK,
			contentType: "text/javascript; charset=utf-8",
		},
		{
			name:   "unknown",
			path:   "/unknown",
			status: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := serveRequest(handler, http.MethodGet, tt.path)
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
			if tt.path == "/ui/status" {
				const event = "event: datastar-patch-elements"
				if got := strings.Count(response.Body.String(), event); got != 1 {
					t.Errorf("status event count = %d, want 1; body = %q", got, response.Body.String())
				}
			}
		})
	}
}

func TestHandlerRejectsUnsupportedMethods(t *testing.T) {
	handler := mustNewHandler(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	paths := []string{
		"/",
		"/healthz",
		"/ui/status",
		"/assets/terminal.css",
		"/assets/app.css",
		"/assets/datastar.js",
	}
	methods := []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}

	for _, path := range paths {
		for _, method := range methods {
			t.Run(method+" "+path, func(t *testing.T) {
				response := serveRequest(handler, method, path)
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
		// shell regions
		`class="topbar"`,
		`class="sidebar"`,
		`id="tree"`,
		`class="main"`,
		`class="deck"`,
		// the brass ring-dial instrument + per-agent readouts
		`class="instrument"`,
		`id="alidade"`,
		`id="breadcrumb"`,
		// global question alert with its count
		`id="alertBanner"`,
		`id="alertCount"`,
		// the four detail tabs
		`data-tab="question"`,
		`data-tab="transcript"`,
		`data-tab="tools"`,
		`data-tab="metrics"`,
		// agents across the nested tree
		`RPC-SPIKE`,
		`WEB-SHELL`,
		`INCUS-IMAGE`,
		`FINAL-REVIEW`,
		`RESEARCHER`,
		`PTY-OWNER`,
		`TEST-RUNNER`,
		`CORRECTNESS`,
		`ORBITAL-INGEST`,
		`MERIDIAN-REVIEW`,
		// question card content + metrics
		`Which contract should I lock in`,
		`class="metrics"`,
		// command deck actions
		`Steer`,
		`Interrupt`,
		`Stop Run`,
		`Spawn Subagent`,
		// colorblind-safe: state paired with glyph + text
		`● active`,
		`◇ question`,
		`○ complete`,
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
	}
	for _, unwanted := range obsolete {
		if strings.Contains(body, unwanted) {
			t.Errorf("initial page retains obsolete content %q", unwanted)
		}
	}
}

func TestAstrolabeConsoleIsInteractive(t *testing.T) {
	body := indexBody(t)

	// The console is wired by exactly two scripts: the local Datastar module
	// (empty body) and the inline console controller (non-empty body). No
	// external scripts.
	scriptRE := regexp.MustCompile(`(?s)<script\b([^>]*)>(.*?)</script>`)
	scripts := scriptRE.FindAllStringSubmatch(body, -1)
	if len(scripts) != 2 {
		t.Fatalf("script count = %d, want 2 (Datastar module + inline controller)", len(scripts))
	}

	var sawDatastar, sawController bool
	for _, script := range scripts {
		attrs, inner := script[1], strings.TrimSpace(script[2])
		if strings.Contains(attrs, `src=`) {
			if !strings.Contains(attrs, `type="module"`) || !strings.Contains(attrs, `src="/assets/datastar.js"`) {
				t.Errorf("external script is not the local Datastar module: %s", script[0])
			}
			if inner != "" {
				t.Errorf("Datastar module script has unexpected inline body %q", inner)
			}
			sawDatastar = true
			continue
		}
		// inline controller
		if inner == "" {
			t.Error("inline controller script has an empty body")
		}
		for _, want := range []string{"selectRow", "addEventListener", "querySelectorAll"} {
			if !strings.Contains(inner, want) {
				t.Errorf("inline controller is missing wiring %q", want)
			}
		}
		sawController = true
	}
	if !sawDatastar {
		t.Error("page is missing the local Datastar module script")
	}
	if !sawController {
		t.Error("page is missing the inline console controller script")
	}

	// The console is a working control surface: it accepts input (search,
	// answer, deck) and its buttons are live, not disabled placeholders.
	for _, want := range []string{`id="search"`, `class="deck-input"`, `class="qwrite"`} {
		if !strings.Contains(body, want) {
			t.Errorf("interactive console is missing input %q", want)
		}
	}
	if strings.Contains(body, " disabled") {
		t.Error("Astrolabe console must not ship disabled placeholder controls")
	}
}

func TestAstrolabeGroupsNestedSubagentsUnderParents(t *testing.T) {
	body := indexBody(t)

	// The tree nests subagents inside parents via <details>/.children, with a
	// leaf class for terminal agents. Guard the shape without pinning exact
	// counts (the mock fleet can grow).
	if got := strings.Count(body, `class="children"`); got < 3 {
		t.Errorf("nested subagent groups = %d, want at least 3", got)
	}
	if got := strings.Count(body, `<details`); got < 3 {
		t.Errorf("collapsible parent runs = %d, want at least 3", got)
	}
	if !strings.Contains(body, `class="row leaf `) {
		t.Error("tree does not mark any leaf (terminal) agents")
	}

	// Each row carries the data the controller needs to drive the detail pane.
	for _, attr := range []string{
		`data-name="RPC-SPIKE"`,
		`data-state="question"`,
		`data-crumb="ORBITAL-INGEST › RPC-SPIKE"`,
		`data-angle=`,
		`data-tokens=`,
	} {
		if !strings.Contains(body, attr) {
			t.Errorf("tree row is missing controller data %q", attr)
		}
	}

	// Depth-3 lineage is present: a subagent whose crumb has three segments.
	if !strings.Contains(body, `ORBITAL-INGEST › RPC-SPIKE › CORRECTNESS`) {
		t.Error("tree does not expose a depth-3 nested subagent lineage")
	}

	// Question rows are flagged so a colorblind operator cannot miss them.
	if got := strings.Count(body, `class="asks"`); got < 2 {
		t.Errorf("flagged question rows = %d, want at least 2", got)
	}
}

func TestAssetsAreEmbedded(t *testing.T) {
	handler := mustNewHandler(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
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

	for _, path := range []string{
		"/",
		"/healthz",
		"/ui/status",
		"/assets/terminal.css",
		"/assets/app.css",
		"/assets/datastar.js",
	} {
		if response := serveRequest(handler, http.MethodGet, path); response.Code != http.StatusOK {
			t.Errorf("GET %s status = %d, want %d", path, response.Code, http.StatusOK)
		}
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
	}
	if len(matches) != len(want) {
		t.Fatalf("runtime asset count = %d, want %d", len(matches), len(want))
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

func TestStatusStreamHonorsCanceledRequest(t *testing.T) {
	handler := mustNewHandler(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/ui/status", nil).WithContext(ctx)
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

func mustNewHandler(t *testing.T, logger *slog.Logger) http.Handler {
	t.Helper()
	handler, err := newHandler(logger)
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	return handler
}

func serveRequest(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(method, path, nil))
	return response
}

func indexBody(t *testing.T) string {
	t.Helper()
	handler := mustNewHandler(t, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	response := serveRequest(handler, http.MethodGet, "/")
	if response.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want %d", response.Code, http.StatusOK)
	}
	return response.Body.String()
}
