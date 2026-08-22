// Package transport adapts HTTP requests to Risk application operations.
package transport

import (
	"net/http"
)

// Handler owns transport concerns for the reference Risk module.
type Handler struct{}

// NewHandler constructs a Risk HTTP handler.
func NewHandler() Handler { return Handler{} }

// ServeHTTP intentionally exposes no feature endpoint yet.
func (Handler) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	http.NotFound(w, nil)
}
