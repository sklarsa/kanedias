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
	"github.com/sklarsa/kanedias/internal/manager"
	"github.com/starfederation/datastar-go/datastar"
)

//go:embed web/*
var webFiles embed.FS

func newHandler(logger *slog.Logger) (http.Handler, error) {
	if logger == nil {
		return nil, errors.New("logger is required")
	}
	return newHandlerWithOptions(logger, "", io.Discard, nil, context.Background(), true)
}

// newHandlerWithOptions builds the full handler with security wiring.
// effectiveAddress is the listener's address used for same-origin checks.
// bootstrapOutput receives the one-time bootstrap token.
// fleet is the manager (may be nil in tests).
// streamCtx is canceled when the server shuts down, closing SSE streams.
// requireSession controls whether a /bootstrap session cookie is required to
// reach the console. The server is loopback-only, so an operator can opt out for
// a single-user console; the same-origin write boundary is always kept.
func newHandlerWithOptions(logger *slog.Logger, effectiveAddress string, bootstrapOutput io.Writer, fleet fleetManager, streamCtx context.Context, requireSession bool) (http.Handler, error) {
	if logger == nil {
		return nil, errors.New("logger is required")
	}

	templates, err := parseTemplates(webFiles)
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}

	// Capability store: prints bootstrap token once to bootstrapOutput. Only
	// created (and required) when session auth is enabled.
	auth := (*capabilityStore)(nil)
	if requireSession {
		var storeErr error
		auth, storeErr = newCapabilityStore(defaultRandom, bootstrapOutput)
		if storeErr != nil {
			return nil, fmt.Errorf("create capability store: %w", storeErr)
		}
	}
	// sessionRequired is a no-op when auth is disabled.
	sessionRequired := func(next http.Handler) http.Handler { return next }
	if auth != nil {
		sessionRequired = auth.requireSession
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
	serveAppJS := serveEmbeddedAsset(logger, "web/app.js", "text/javascript; charset=utf-8")

	router := chi.NewRouter()
	router.Use(requestLogger(logger), recoverPanics(logger))

	// Unauthenticated routes.
	router.Get("/healthz", serveHealth)
	if auth != nil {
		router.Get("/bootstrap", auth.serveBootstrap)
	}
	router.Get("/assets/terminal.css", serveTerminalCSS)
	router.Get("/assets/app.css", serveCSS)
	router.Get("/assets/datastar.js", serveJavaScript)
	router.Get("/assets/app.js", serveAppJS)

	var serveFleet, serveSession http.HandlerFunc
	if fleet != nil {
		serveFleet = makeFleetHandler(fleet, templates, logger, streamCtx)
		serveSession = makeSessionHandler(fleet, templates, logger, streamCtx)
	} else {
		serveFleet = http.NotFoundHandler().ServeHTTP
		serveSession = http.NotFoundHandler().ServeHTTP
	}

	// Protected routes (require session cookie when auth is enabled).
	router.Group(func(protected chi.Router) {
		protected.Use(sessionRequired)
		protected.Get("/", serveIndex)
		protected.Route("/ui", func(ui chi.Router) {
			ui.Get("/status", serveStatus)
			ui.Get("/fleet", serveFleet)
			ui.Get("/session", serveSession)
		})
	})

	// Write boundary applied to all action POSTs.
	router.Group(func(write chi.Router) {
		write.Use(sessionRequired)
		write.Use(boundary.requireWriteBoundary)
		if fleet != nil {
			write.Post("/ui/sessions", makeNewSessionHandler(fleet, templates, logger))
			write.Post("/ui/sessions/{sessionID}/steer", makeSteerHandler(fleet, templates, logger))
			write.Post("/ui/sessions/{sessionID}/interrupt", makeInterruptHandler(fleet, templates, logger))
			write.Post("/ui/sessions/{sessionID}/stop", makeStopSessionHandler(fleet, templates, logger))
			write.Post("/ui/sessions/{sessionID}/questions/{questionID}", makeAnswerQuestionHandler(fleet, templates, logger))
		} else {
			write.Post("/ui/sessions", http.NotFoundHandler().ServeHTTP)
			write.Post("/ui/sessions/{sessionID}/steer", http.NotFoundHandler().ServeHTTP)
			write.Post("/ui/sessions/{sessionID}/interrupt", http.NotFoundHandler().ServeHTTP)
			write.Post("/ui/sessions/{sessionID}/stop", http.NotFoundHandler().ServeHTTP)
			write.Post("/ui/sessions/{sessionID}/questions/{questionID}", http.NotFoundHandler().ServeHTTP)
		}
	})

	return router, nil
}

// makeFleetHandler returns a handler that streams fleet updates to the browser.
// It subscribes before reading the initial snapshot to avoid race conditions.
func makeFleetHandler(fleet fleetManager, templates *template.Template, logger *slog.Logger, serverStreams context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		subscription := fleet.SubscribeFleet()
		defer subscription.Close()

		initial, err := renderTemplate(templates, templateFleet, newFleetView(fleet.Fleet()))
		if err != nil {
			logger.Error("render fleet template", "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		streamCtx, cancel := mergeStreamContext(r.Context(), serverStreams)
		defer cancel()

		sse := datastar.NewSSE(w, r, datastar.WithContext(streamCtx))
		if err := sse.PatchElements(initial, datastar.WithSelectorID("fleet-panel"), datastar.WithModeOuter()); err != nil {
			return
		}

		for {
			select {
			case <-streamCtx.Done():
				return
			case _, open := <-subscription.Updates:
				if !open {
					return
				}
				if err := patchTemplate(sse, templates, templateFleet, "fleet-panel", newFleetView(fleet.Fleet())); err != nil {
					return
				}
			}
		}
	}
}

// makeSessionHandler returns a handler that streams selected session details.
func makeSessionHandler(fleet fleetManager, templates *template.Template, logger *slog.Logger, serverStreams context.Context) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		signals, err := decodeSignals[selectedSessionSignals](w, r)
		if err != nil {
			logger.Error("decode session signals", "error", err)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}

		sessionID := signals.SelectedSessionID
		if sessionID == "" {
			// Nothing selected yet; render empty panels.
			streamCtx, cancel := mergeStreamContext(r.Context(), serverStreams)
			defer cancel()
			sse := datastar.NewSSE(w, r, datastar.WithContext(streamCtx))
			emptyState := emptySessionState()
			_ = patchTemplate(sse, templates, templateDetail, "detail-panel", newDetailView(emptyState, statsView{}))
			_ = patchTemplate(sse, templates, templateQuestions, "question-panel", newQuestionPanelView(emptyState))
			_ = patchTemplate(sse, templates, templateActivity, "activity-panel", newActivityView(emptyState))
			return
		}

		subscription, err := fleet.SubscribeSession(sessionID)
		if err != nil {
			logger.Error("subscribe session", "sessionID", sessionID, "error", err)
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		defer subscription.Close()

		state, err := fleet.Session(sessionID)
		if err != nil {
			logger.Error("fetch session state", "sessionID", sessionID, "error", err)
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}

		streamCtx, cancel := mergeStreamContext(r.Context(), serverStreams)
		defer cancel()

		sse := datastar.NewSSE(w, r, datastar.WithContext(streamCtx))

		// Throttled stats fetcher: at most one get_session_stats per second,
		// only for actionable, non-stale nodes. The last successful stats are
		// reused between refreshes so a burst of activity revisions does not turn
		// into a burst of stats RPCs.
		statsFetcher := newStatsFetcher(streamCtx, fleet, sessionID, logger)

		// Initial render.
		if err := patchSessionTargets(sse, templates, state, statsFetcher.get(state)); err != nil {
			return
		}

		// Rate-limit activity coalescing.
		activityCoalesce := time.NewTicker(50 * time.Millisecond)
		defer activityCoalesce.Stop()
		pendingActivity := false

		for {
			select {
			case <-streamCtx.Done():
				return
			case _, open := <-subscription.Updates:
				if !open {
					return
				}
				state, err = fleet.Session(sessionID)
				if err != nil {
					return
				}
				if err := patchTemplate(sse, templates, templateDetail, "detail-panel", newDetailView(state, statsFetcher.get(state))); err != nil {
					return
				}
				if err := patchTemplate(sse, templates, templateQuestions, "question-panel", newQuestionPanelView(state)); err != nil {
					return
				}
				pendingActivity = true
			case <-activityCoalesce.C:
				if pendingActivity {
					pendingActivity = false
					if err := patchTemplate(sse, templates, templateActivity, "activity-panel", newActivityView(state)); err != nil {
						return
					}
				}
			}
		}
	}
}

// statsThrottle bounds how often get_session_stats is called per session stream.
const statsThrottle = time.Second

// sessionStatsFetcher fetches SessionStats subject to a per-stream throttle and
// reuses the last successful result between refreshes.
type sessionStatsFetcher struct {
	ctx       context.Context
	fleet     fleetManager
	sessionID string
	logger    *slog.Logger
	now       func() time.Time

	haveStats bool
	last      statsView
	lastFetch time.Time
	fetched   bool
}

func newStatsFetcher(ctx context.Context, fleet fleetManager, sessionID string, logger *slog.Logger) *sessionStatsFetcher {
	return &sessionStatsFetcher{
		ctx:       ctx,
		fleet:     fleet,
		sessionID: sessionID,
		logger:    logger,
		now:       time.Now,
	}
}

// get returns the stats view for the current state, fetching fresh stats at most
// once per statsThrottle. It skips fetching for stale or non-actionable nodes and
// falls back to the last successful stats (or an empty view rendering "—").
func (s *sessionStatsFetcher) get(state manager.SessionState) statsView {
	if !actionableForStats(state) {
		return s.last
	}
	// Throttle: reuse the last result unless a full interval has elapsed since
	// the previous fetch attempt.
	if s.fetched && s.now().Sub(s.lastFetch) < statsThrottle {
		return s.last
	}
	s.fetched = true
	s.lastFetch = s.now()
	stats, err := s.fleet.SessionStats(s.ctx, s.sessionID)
	if err != nil {
		if s.logger != nil && s.ctx.Err() == nil {
			s.logger.Debug("fetch session stats", "sessionID", s.sessionID, "error", err)
		}
		// Keep the last successful stats (or empty view) rather than clearing.
		return s.last
	}
	s.haveStats = true
	s.last = newStatsView(stats)
	return s.last
}

// actionableForStats reports whether a session is a candidate for a stats fetch:
// its root must not be stale and the node lifecycle must be one Pi can serve
// get_session_stats for (ready/running/awaiting-handoff/question). Terminal
// lifecycles are skipped to avoid pointless RPCs.
func actionableForStats(state manager.SessionState) bool {
	if state.RootStale {
		return false
	}
	switch state.Node.Lifecycle {
	case "stopping", "failed", "stopped", "completed", "":
		return false
	default:
		return true
	}
}

// patchSessionTargets patches all three session-detail targets.
func patchSessionTargets(sse *datastar.ServerSentEventGenerator, templates *template.Template, state manager.SessionState, stats statsView) error {
	if err := patchTemplate(sse, templates, templateDetail, "detail-panel", newDetailView(state, stats)); err != nil {
		return err
	}
	if err := patchTemplate(sse, templates, templateQuestions, "question-panel", newQuestionPanelView(state)); err != nil {
		return err
	}
	return patchTemplate(sse, templates, templateActivity, "activity-panel", newActivityView(state))
}

func parseTemplates(fsys fs.FS) (*template.Template, error) {
	return template.ParseFS(fsys, "web/index.html", "web/fleet.html", "web/detail.html",
		"web/questions.html", "web/activity.html", "web/deck-status.html")
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
