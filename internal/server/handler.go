package server

import (
	"log/slog"
	"net/http"
)

func newHandler(*slog.Logger) (http.Handler, error) {
	return http.NotFoundHandler(), nil
}
