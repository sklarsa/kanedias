package supervisorapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/sklarsa/kanedias/internal/supervisor"
	"github.com/sklarsa/kanedias/internal/supervisor/contract"
)

const MaxRequestBodyBytes = 1 << 20

type Service interface {
	Snapshot(context.Context) (supervisor.NodeSnapshot, error)
	Workers(context.Context) []contract.WorkerSummary
	CallRPC(context.Context, string, json.RawMessage) (json.RawMessage, error)
	AnswerQuestion(context.Context, string, string, json.RawMessage) error
	Subscribe(context.Context) (supervisor.Subscription, error)
	Stop(context.Context, string) error
}

func NewHandler(service Service) http.Handler {
	router := chi.NewRouter()
	router.NotFound(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, contract.NewError(contract.ErrorNotFound, "route not found"))
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusMethodNotAllowed, contract.Error{Code: contract.ErrorInvalidRequest, Message: "method not allowed"})
	})

	router.Get("/v1/tree", func(w http.ResponseWriter, request *http.Request) {
		snapshot, err := service.Snapshot(request.Context())
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	})
	router.Get("/v1/workers", func(w http.ResponseWriter, request *http.Request) {
		writeJSON(w, http.StatusOK, service.Workers(request.Context()))
	})
	router.Get("/v1/events", func(w http.ResponseWriter, request *http.Request) {
		serveEvents(w, request, service)
	})
	router.Post("/v1/sessions/{sessionID}/rpc", func(w http.ResponseWriter, request *http.Request) {
		body, err := readJSONBody(w, request)
		if err != nil {
			writeError(w, err)
			return
		}
		response, err := service.CallRPC(request.Context(), chi.URLParam(request, "sessionID"), body)
		if err != nil {
			writeError(w, err)
			return
		}
		writeRawJSON(w, http.StatusOK, response)
	})
	router.Post("/v1/sessions/{sessionID}/questions/{questionID}/response", func(w http.ResponseWriter, request *http.Request) {
		body, err := readJSONBody(w, request)
		if err != nil {
			writeError(w, err)
			return
		}
		if err := service.AnswerQuestion(request.Context(), chi.URLParam(request, "sessionID"), chi.URLParam(request, "questionID"), body); err != nil {
			writeError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	router.Delete("/v1/sessions/{sessionID}", func(w http.ResponseWriter, request *http.Request) {
		sessionID := chi.URLParam(request, "sessionID")
		// Complete the response before a self-stop can tear down this connection.
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "stopping"})
		go func(ctx context.Context) { _ = service.Stop(ctx, sessionID) }(context.WithoutCancel(request.Context()))
	})
	return router
}

func readJSONBody(w http.ResponseWriter, request *http.Request) (json.RawMessage, error) {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, &httpBoundaryError{status: http.StatusUnsupportedMediaType, err: contract.NewError(contract.ErrorInvalidRequest, "Content-Type must be application/json")}
	}
	request.Body = http.MaxBytesReader(w, request.Body, MaxRequestBodyBytes)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, &httpBoundaryError{status: http.StatusRequestEntityTooLarge, err: contract.NewError(contract.ErrorInvalidRequest, "request body exceeds 1 MiB")}
		}
		return nil, contract.NewError(contract.ErrorInvalidRequest, "read request body: "+err.Error())
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 || !json.Valid(body) {
		return nil, contract.NewError(contract.ErrorInvalidRequest, "request body must contain exactly one JSON value")
	}
	return append(json.RawMessage(nil), body...), nil
}

type httpBoundaryError struct {
	status int
	err    error
}

func (err *httpBoundaryError) Error() string { return err.err.Error() }
func (err *httpBoundaryError) Unwrap() error { return err.err }

func writeError(w http.ResponseWriter, err error) {
	status := contract.HTTPStatus(err)
	var boundary *httpBoundaryError
	if errors.As(err, &boundary) {
		status = boundary.status
		err = boundary.err
	}
	var typed *contract.Error
	if !errors.As(err, &typed) {
		typed = contract.NewError(contract.ErrorInternal, "internal supervisor error")
	}
	writeJSON(w, status, typed)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeRawJSON(w http.ResponseWriter, status int, raw json.RawMessage) {
	if !json.Valid(raw) {
		writeError(w, errors.New("service returned invalid JSON"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		_, _ = w.Write([]byte("\n"))
	}
}
