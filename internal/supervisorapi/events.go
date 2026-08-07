package supervisorapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sklarsa/kanedias/internal/supervisor"
)

func serveEvents(w http.ResponseWriter, request *http.Request, service Service) {
	subscription, err := service.Subscribe(request.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	if subscription.Close != nil {
		defer subscription.Close()
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, fmt.Errorf("streaming is unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	write := func(event supervisor.EventEnvelope) error {
		wire, err := json.Marshal(event)
		if err != nil {
			return err
		}
		kind := strings.NewReplacer("\r", "", "\n", "").Replace(event.Kind)
		if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Seq, kind, wire); err != nil {
			return err
		}
		flusher.Flush()
		return nil
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
