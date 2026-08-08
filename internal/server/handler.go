package server

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/starfederation/datastar-go/datastar"
)

//go:embed web/*
var webFiles embed.FS

func newHandler(logger *slog.Logger) (http.Handler, error) {
	if logger == nil {
		return nil, errors.New("logger is required")
	}
	return newHandlerWithOptions(logger, "", io.Discard)
}

// newHandlerWithOptions builds the full handler with security wiring.
// effectiveAddress is the listener's address used for same-origin checks.
// bootstrapOutput receives the one-time bootstrap token.
func newHandlerWithOptions(logger *slog.Logger, effectiveAddress string, bootstrapOutput io.Writer) (http.Handler, error) {
	if logger == nil {
		return nil, errors.New("logger is required")
	}

	templates, err := parseTemplates(webFiles)
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}

	// Capability store: prints bootstrap token once to bootstrapOutput.
	auth, err := newCapabilityStore(defaultRandom, bootstrapOutput)
	if err != nil {
		return nil, fmt.Errorf("create capability store: %w", err)
	}

	boundary := newRequestBoundary(effectiveAddress)

	serveIndex := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := templates.ExecuteTemplate(w, "index.html", nil); err != nil {
			logger.Error("render index", "error", err)
		}
	}
	serveHealth := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	}
	serveStatus := func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() != nil {
			return
		}
		sse := datastar.NewSSE(w, r)
		if err := sse.PatchElements(`<output id="server-status" role="status">Running</output>`); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("send status event", "error", err)
		}
	}
	serveTerminalCSS := serveEmbeddedAsset(logger, "web/terminal.css", "text/css; charset=utf-8")
	serveCSS := serveEmbeddedAsset(logger, "web/app.css", "text/css; charset=utf-8")
	serveJavaScript := serveEmbeddedAsset(logger, "web/datastar.js", "text/javascript; charset=utf-8")

	router := chi.NewRouter()
	router.Use(requestLogger(logger), recoverPanics(logger))

	// Unauthenticated routes.
	router.Get("/healthz", serveHealth)
	router.Get("/bootstrap", auth.serveBootstrap)
	router.Get("/assets/terminal.css", serveTerminalCSS)
	router.Get("/assets/app.css", serveCSS)
	router.Get("/assets/datastar.js", serveJavaScript)

	// Protected routes (require session cookie).
	router.Group(func(protected chi.Router) {
		protected.Use(auth.requireSession)
		protected.Get("/", serveIndex)
		protected.Route("/ui", func(ui chi.Router) {
			ui.Get("/status", serveStatus)
			ui.Get("/fleet", http.NotFoundHandler().ServeHTTP)
			ui.Get("/session", http.NotFoundHandler().ServeHTTP)
		})
	})

	// Write boundary applied to all action POSTs.
	router.Group(func(write chi.Router) {
		write.Use(auth.requireSession)
		write.Use(boundary.requireWriteBoundary)
		write.Post("/ui/sessions", http.NotFoundHandler().ServeHTTP)
		write.Post("/ui/sessions/{sessionID}/steer", http.NotFoundHandler().ServeHTTP)
		write.Post("/ui/sessions/{sessionID}/interrupt", http.NotFoundHandler().ServeHTTP)
		write.Post("/ui/sessions/{sessionID}/stop", http.NotFoundHandler().ServeHTTP)
		write.Post("/ui/sessions/{sessionID}/questions/{questionID}", http.NotFoundHandler().ServeHTTP)
	})

	return router, nil
}

func parseTemplates(fsys fs.FS) (*template.Template, error) {
	return template.ParseFS(fsys, "web/index.html")
}

func serveEmbeddedAsset(logger *slog.Logger, name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		contents, err := webFiles.ReadFile(name)
		if err != nil {
			logger.Error("read embedded web asset", "error", err, "asset", name)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(contents)
	}
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			wrapped := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(wrapped, r)
			status := wrapped.Status()
			if status == 0 {
				status = http.StatusOK
			}
			logger.Info("request completed",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration", time.Since(started),
				"remote_addr", r.RemoteAddr,
			)
		})
	}
}

func recoverPanics(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error("panic recovered", "panic", recovered)
					http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
