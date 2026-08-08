package server

import (
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/starfederation/datastar-go/datastar"
)

// patchDeckStatus renders the deck-status template and patches #deck-status
// on the SSE stream. err may be nil (success) or non-nil (mapped to operator copy).
func patchDeckStatusAction(w http.ResponseWriter, r *http.Request, templates *template.Template, logger *slog.Logger, err error) {
	view := newDeckStatusView(err)
	html, renderErr := renderTemplate(templates, templateDeckStatus, view)
	if renderErr != nil {
		logger.Error("render deck-status template", "error", renderErr)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	sse := datastar.NewSSE(w, r)
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
			logger.Error("decode steer signals", "error", err)
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

// makeNewSessionHandler returns the handler for POST /ui/sessions.
func makeNewSessionHandler(fleet fleetManager, templates *template.Template, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := fleet.SpawnRoot(r.Context())
		patchDeckStatusAction(w, r, templates, logger, err)
	}
}

// makeAnswerQuestionHandler returns the handler for POST /ui/sessions/{sessionID}/questions/{questionID}.
func makeAnswerQuestionHandler(fleet fleetManager, templates *template.Template, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := chi.URLParam(r, "sessionID")
		questionID := chi.URLParam(r, "questionID")
		signals, err := decodeSignals[answerSignals](w, r)
		if err != nil {
			logger.Error("decode answer signals", "error", err)
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
