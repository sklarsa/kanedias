package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/starfederation/datastar-go/datastar"
)

// steerSignals carries the message from a Steer action.
type steerSignals struct {
	Message string `json:"message"`
}

// selectedSessionSignals carries the selected session ID.
type selectedSessionSignals struct {
	SelectedSessionID string `json:"selectedSessionId"`
}

// answerSignals carries one of the allowed answer shapes for question responses.
type answerSignals struct {
	Value     *string `json:"value,omitempty"`
	Confirmed *bool   `json:"confirmed,omitempty"`
	Cancelled bool    `json:"cancelled,omitempty"`
}

// decodeJSON reads a direct JSON request body capped at 64 KiB. Unknown fields
// and trailing JSON values are rejected.
func decodeJSON[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var zero T
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return zero, fmt.Errorf("request body contains multiple JSON values")
	}
	return value, nil
}

// decodeSignals reads and strictly decodes Datastar signals from the request.
// For non-GET requests the body is capped at 64 KiB.
// Unknown fields and trailing JSON are rejected.
func decodeSignals[T any](w http.ResponseWriter, r *http.Request) (T, error) {
	var zero T
	if r.Method != http.MethodGet {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	}
	var raw json.RawMessage
	if err := datastar.ReadSignals(r, &raw); err != nil {
		return zero, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return zero, fmt.Errorf("signals contain multiple JSON values")
	}
	return value, nil
}
