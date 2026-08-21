// Package transport adapts HTTP requests to Risk application operations.
package transport

import (
	"net/http"

	"lidradar/backend/internal/risk/application"
)

// Handler owns transport concerns for the reference Risk module.
type Handler struct {
	register application.Register
}

// NewHandler constructs a Risk HTTP handler.
func NewHandler(register application.Register) Handler {
	return Handler{register: register}
}

// ServeHTTP intentionally exposes no feature endpoint yet.
func (Handler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	http.NotFound(w, nil)
}
