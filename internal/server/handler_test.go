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
			contains:    []string{"<title>Kanedias</title>", "Refresh status", "Not refreshed yet."},
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

func TestInitialPageContainsInertPanels(t *testing.T) {
	body := indexBody(t)
	panels := []string{
		`<section id="dashboard-panel" aria-labelledby="dashboard-heading">
  <h2 id="dashboard-heading">Dashboard</h2>
  <p>Dashboard view is not available in this scaffold.</p>
</section>`,
		`<section id="session-panel" aria-labelledby="session-heading">
  <h2 id="session-heading">Sessions</h2>
  <p>Session view is not available in this scaffold.</p>
</section>`,
	}
	for _, panel := range panels {
		if !strings.Contains(body, panel) {
			t.Errorf("initial page is missing exact inert panel:\n%s", panel)
		}
	}

	sectionRE := regexp.MustCompile(`(?s)<section id="(?:dashboard|session)-panel".*?</section>`)
	sections := sectionRE.FindAllString(body, -1)
	if len(sections) != 2 {
		t.Fatalf("found %d future-view panels, want 2", len(sections))
	}
	for _, section := range sections {
		lower := strings.ToLower(section)
		for _, forbidden := range []string{"<form", "<a ", "<button", "data-on:", "data-init", "incus", "delete", "create", "update"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("future-view panel contains forbidden interactive or mutable content %q: %s", forbidden, section)
			}
		}
	}
	if got := strings.Count(strings.ToLower(body), "<button"); got != 1 {
		t.Errorf("interactive button count = %d, want 1", got)
	}
	for _, tag := range []string{"<form", "<select", "<textarea", "<input", "<a "} {
		if strings.Contains(strings.ToLower(body), tag) {
			t.Errorf("page contains unexpected interactive element %q", tag)
		}
	}
}

func TestIndexRequiresClickForStatusRefresh(t *testing.T) {
	body := indexBody(t)
	const binding = `data-on:click="@get('/ui/status')"`
	if got := strings.Count(body, "/ui/status"); got != 1 {
		t.Errorf("/ui/status occurrence count = %d, want 1", got)
	}
	if got := strings.Count(body, binding); got != 1 {
		t.Errorf("status click binding count = %d, want 1", got)
	}
	buttonRE := regexp.MustCompile(`(?s)<button\b[^>]*>.*?</button>`)
	buttons := buttonRE.FindAllString(body, -1)
	if len(buttons) != 1 || !strings.Contains(buttons[0], binding) || !strings.Contains(buttons[0], "Refresh status") {
		t.Errorf("visible refresh button does not have the sole click binding: %q", buttons)
	}
	lower := strings.ToLower(body)
	for _, forbidden := range []string{"data-init", "fetch(", "xmlhttprequest"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("page contains automatic request mechanism %q", forbidden)
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

func TestRenderedPageHasNoExternalRuntimeAssets(t *testing.T) {
	body := indexBody(t)
	assetRE := regexp.MustCompile(`(?:src|href)="([^"]+)"`)
	assets := assetRE.FindAllStringSubmatch(body, -1)
	if len(assets) != 2 {
		t.Fatalf("runtime asset count = %d, want 2", len(assets))
	}
	for _, match := range assets {
		asset := strings.ToLower(match[1])
		if strings.HasPrefix(asset, "http://") || strings.HasPrefix(asset, "https://") || strings.HasPrefix(asset, "//") || strings.Contains(asset, "cdn") || strings.Contains(asset, "node_modules") || strings.Contains(asset, "npm") || strings.Contains(asset, "unpkg") {
			t.Errorf("external runtime asset %q", match[1])
		}
		if !strings.HasPrefix(asset, "/assets/") {
			t.Errorf("runtime asset is not local: %q", match[1])
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
