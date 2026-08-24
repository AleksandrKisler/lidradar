package httpplatform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

const (
	requestIDHeader   = "X-Request-ID"
	traceparentHeader = "Traceparent"
)

type correlationKey string

const (
	requestIDKey correlationKey = "request_id"
	traceIDKey   correlationKey = "trace_id"
)

var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func correlation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get(requestIDHeader)
		if !safeRequestID.MatchString(requestID) {
			requestID = randomHex(12)
		}
		traceID := traceIDFromTraceparent(r.Header.Get(traceparentHeader))
		if traceID == "" {
			traceID = randomHex(16)
		}
		ctx := context.WithValue(r.Context(), requestIDKey, requestID)
		ctx = context.WithValue(ctx, traceIDKey, traceID)
		w.Header().Set(requestIDHeader, requestID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func recovery(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if recovered := recover(); recovered != nil {
					logger.Error("HTTP panic recovered", "event", "http.panic", "request_id", RequestID(r.Context()), "trace_id", TraceID(r.Context()))
					WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Internal server error", nil)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func requestLogging(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()
			recorder := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(recorder, r)
			logger.Info("HTTP request completed",
				"event", "http.request.completed",
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.Status(),
				"duration_ms", time.Since(started).Milliseconds(),
				"request_id", RequestID(r.Context()),
				"trace_id", TraceID(r.Context()),
			)
		})
	}
}

// RequestID returns the request correlation identifier.
func RequestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey).(string)
	return value
}

// TraceID returns the distributed trace identifier.
func TraceID(ctx context.Context) string {
	value, _ := ctx.Value(traceIDKey).(string)
	return value
}

func traceIDFromTraceparent(value string) string {
	parts := strings.Split(value, "-")
	if len(parts) != 4 || len(parts[1]) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil || parts[1] == strings.Repeat("0", 32) {
		return ""
	}
	return strings.ToLower(parts[1])
}

func randomHex(bytes int) string {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return strings.Repeat("0", bytes*2)
	}
	return hex.EncodeToString(buffer)
}
