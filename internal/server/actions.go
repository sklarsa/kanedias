package server

import (
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/sklarsa/kanedias/internal/manager"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
	"github.com/starfederation/datastar-go/datastar"
)

// patchDeckStatus renders the deck-status template and patches #deck-status
// on the SSE stream. err may be nil (success) or non-nil (mapped to operator copy).
//
// The operator only ever sees the sanitized operatorMessage, so the underlying
// error is logged here (server-side only) with request context; otherwise a
// failed command surfaces as generic copy with no record of the real cause.
func patchDeckStatusAction(w http.ResponseWriter, r *http.Request, templates *template.Template, logger *slog.Logger, err error) {
	if err != nil {
		logger.Error("action failed",
			"method", r.Method, "path", r.URL.Path, "error", err)
	}
	view := newDeckStatusView(err)
	html, renderErr := renderTemplate(templates, templateDeckStatus, view)
	if renderErr != nil {
		logger.Error("render deck-status template", "error", renderErr)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	sse := datastar.NewSSE(w, r)
	// Outer mode morphs the complete rendered #deck-status wrapper onto the
	// existing same-ID root, keeping that stable root in place while the client
	// clears the transient success span (and its marker). Inner mode would nest
	// the returned wrapper under its own same-ID root and throw a
	// HierarchyRequestError, so it must not be used here.
	if sseErr := sse.PatchElements(html, datastar.WithSelectorID("deck-status"), datastar.WithModeOuter()); sseErr != nil {
		logger.Error("patch deck-status", "error", sseErr)
	}
}

// makeSteerHandler returns the handler for POST /ui/sessions/{sessionID}/steer.
func makeSteerHandler(fleet fleetManager, templates *template.Template, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := chi.URLParam(r, "sessionID")
		signals, err := decodeSignals[steerSignals](w, r)
		if err != nil {
			patchDeckStatusAction(w, r, templates, logger, err)
			return
		}
		err = fleet.Steer(r.Context(), sessionID, signals.Message)
		patchDeckStatusAction(w, r, templates, logger, err)
	}
}

// makeInterruptHandler returns the handler for POST /ui/sessions/{sessionID}/interrupt.
func makeInterruptHandler(fleet fleetManager, templates *template.Template, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := chi.URLParam(r, "sessionID")
		err := fleet.Interrupt(r.Context(), sessionID)
		patchDeckStatusAction(w, r, templates, logger, err)
	}
}

// makeStopSessionHandler returns the handler for POST /ui/sessions/{sessionID}/stop.
func makeStopSessionHandler(fleet fleetManager, templates *template.Template, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := chi.URLParam(r, "sessionID")
		err := fleet.StopSession(r.Context(), sessionID)
		patchDeckStatusAction(w, r, templates, logger, err)
	}
}

// makeNewSessionHandler returns the strict direct-JSON handler for POST /ui/sessions.
func makeNewSessionHandler(fleet fleetManager, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		request, err := decodeJSON[manager.SessionLaunchRequest](w, r)
		if err != nil {
			writeLaunchJSON(w, http.StatusBadRequest, map[string]string{"error": "The session configuration was not valid."})
			return
		}

		sessionID, err := fleet.SpawnRootWithRequest(r.Context(), request)
		if err != nil {
			logger.Error("launch session failed", "method", r.Method, "path", r.URL.Path, "error", err)
			status := http.StatusServiceUnavailable
			message := "The session could not be started."
			var contractErr *contract.Error
			if errors.As(err, &contractErr) {
				switch contractErr.Code {
				case contract.ErrorInvalidRequest:
					status = http.StatusBadRequest
					message = "The session configuration was not valid."
				case contract.ErrorWorkspaceRepositoryUnavailable:
					message = "The selected repository is not present in the workspace."
				}
			}
			writeLaunchJSON(w, status, map[string]string{"error": message})
			return
		}
		writeLaunchJSON(w, http.StatusCreated, map[string]string{"sessionId": sessionID})
	}
}

func writeLaunchJSON(w http.ResponseWriter, status int, value map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// makeAnswerQuestionHandler returns the handler for POST /ui/sessions/{sessionID}/questions/{questionID}.
func makeAnswerQuestionHandler(fleet fleetManager, templates *template.Template, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := chi.URLParam(r, "sessionID")
		questionID := chi.URLParam(r, "questionID")
		signals, err := decodeSignals[answerSignals](w, r)
		if err != nil {
			patchDeckStatusAction(w, r, templates, logger, err)
			return
		}
		// Build the answer payload.
		var answer json.RawMessage
		switch {
		case signals.Cancelled:
			answer, _ = json.Marshal(map[string]any{"cancelled": true})
		case signals.Confirmed != nil:
			answer, _ = json.Marshal(map[string]any{"confirmed": *signals.Confirmed})
		case signals.Value != nil:
			answer, _ = json.Marshal(map[string]any{"value": *signals.Value})
		default:
			answer = json.RawMessage(`{}`)
		}
		err = fleet.AnswerQuestion(r.Context(), sessionID, questionID, answer)
		patchDeckStatusAction(w, r, templates, logger, err)
	}
}
