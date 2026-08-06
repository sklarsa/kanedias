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
				"KANEDIAS // CIRCLE OF THE FLEET",
				"STATIC DEMONSTRATION",
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

func TestInitialPageContainsCircleOfFleetMockup(t *testing.T) {
	body := indexBody(t)
	required := []string{
		`<html lang="en" data-theme="dark">`,
		`<body class="terminal">`,
		`KANEDIAS // CIRCLE OF THE FLEET`,
		`STATIC DEMONSTRATION`,
		`id="question-alert"`,
		`2 QUESTIONS`,
		`id="fleet-orbit"`,
		`RPC-SPIKE`,
		`WEB-SHELL`,
		`PTY-OWNER`,
		`id="maker-aperture"`,
		`Should shell sessions survive a browser reconnect`,
		`12.8K`,
		`TOKENS`,
		`id="command-deck"`,
		`● ACTIVE`,
		`◇ QUESTION`,
		`○ COMPLETE`,
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("initial page does not contain %q", want)
		}
	}

	obsolete := []string{
		"Refresh status",
		"Not refreshed yet.",
		"Dashboard view is not available in this scaffold.",
		"Session view is not available in this scaffold.",
		`id="dashboard-panel"`,
		`id="session-panel"`,
	}
	for _, unwanted := range obsolete {
		if strings.Contains(body, unwanted) {
			t.Errorf("initial page retains obsolete content %q", unwanted)
		}
	}
}

func TestCircleOfFleetMockupIsStatic(t *testing.T) {
	body := indexBody(t)
	lower := strings.ToLower(body)
	forbidden := []string{
		"data-on:",
		"data-init",
		"@get(",
		"/ui/status",
		"fetch(",
		"xmlhttprequest",
		"onclick=",
		"contenteditable",
		"<form",
		"<input",
		"<textarea",
		"<select",
		"<a ",
	}
	for _, unwanted := range forbidden {
		if strings.Contains(lower, unwanted) {
			t.Errorf("static mockup contains active mechanism %q", unwanted)
		}
	}

	buttonRE := regexp.MustCompile(`(?s)<button\b[^>]*>.*?</button>`)
	buttons := buttonRE.FindAllString(body, -1)
	if len(buttons) != 7 {
		t.Fatalf("button count = %d, want 7", len(buttons))
	}
	for _, button := range buttons {
		if !strings.Contains(button, " disabled") {
			t.Errorf("mockup button is not disabled: %s", button)
		}
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

	digest := sha256.Sum256(stylesheet)
	checks := []string{
		"Commit: 63551f0de711f2f634a0c2da7bab1d3bae216fef",
		fmt.Sprintf("SHA-256: %x", digest),
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

func TestProjectStylesDefineStaticCircleVisualSystem(t *testing.T) {
	contents, err := webFiles.ReadFile("web/app.css")
	if err != nil {
		t.Fatalf("read embedded project stylesheet: %v", err)
	}
	styles := string(contents)
	required := []string{
		"--page-bg: #05070b",
		"--active: #69a9ed",
		"--question: #d9ae70",
		"min-width: 72rem",
		"#fleet-orbit",
		".orbit-ring",
		".run-node",
		".child-moon",
		"#maker-aperture",
		"#question-alert",
		"#command-deck",
	}
	for _, want := range required {
		if !strings.Contains(styles, want) {
			t.Errorf("project stylesheet does not contain %q", want)
		}
	}

	lower := strings.ToLower(styles)
	for _, unwanted := range []string{"@import", "http://", "https://", "url(", "@keyframes", "animation:"} {
		if strings.Contains(lower, unwanted) {
			t.Errorf("project stylesheet contains disallowed runtime or motion construct %q", unwanted)
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
