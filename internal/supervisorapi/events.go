package supervisorapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

const supervisorSSEWriteTimeout = time.Second

func withSupervisorSSEWriteDeadline(controller *http.ResponseController, operation func() error) error {
	deadlineSet := true
	if err := controller.SetWriteDeadline(time.Now().Add(supervisorSSEWriteTimeout)); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		deadlineSet = false
	}
	clearDeadline := func(operationErr error) error {
		if !deadlineSet {
			return operationErr
		}
		return errors.Join(operationErr, controller.SetWriteDeadline(time.Time{}))
	}

	if err := operation(); err != nil {
		return clearDeadline(err)
	}
	return clearDeadline(controller.Flush())
}

func serveEvents(w http.ResponseWriter, request *http.Request, service Service) {
	subscription, err := service.Subscribe(request.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if subscription.Close != nil {
		defer subscription.Close()
	}

	if _, ok := w.(http.Flusher); !ok {
		writeError(w, fmt.Errorf("streaming is unsupported"))
		return
	}
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := withSupervisorSSEWriteDeadline(controller, func() error {
		w.WriteHeader(http.StatusOK)
		return nil
	}); err != nil {
		return
	}

	write := func(event supervisor.EventEnvelope) error {
		wire, err := json.Marshal(event)
		if err != nil {
			return err
		}
		kind := strings.NewReplacer("\r", "", "\n", "").Replace(event.Kind)
		return withSupervisorSSEWriteDeadline(controller, func() error {
			_, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, kind, wire)
			return err
		})
	}
	for _, event := range subscription.Replay {
		if write(event) != nil {
			return
		}
	}
	for {
		select {
		case <-request.Context().Done():
			return
		case event, open := <-subscription.Events:
			if !open {
				return
			}
			if write(event) != nil {
				return
			}
		}
	}
}
