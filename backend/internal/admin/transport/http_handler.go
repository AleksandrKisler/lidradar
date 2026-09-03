// Package transport публикует административный API под сессией платформенного
// администратора; заголовок организации не требуется.
package transport

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/admin/application"
	httpplatform "lidradar/backend/platform/http"
)

type UserResolver interface {
	User(*http.Request) (string, bool, error)
}

type Handler struct {
	service application.Service
	users   UserResolver
}

func NewHandler(service application.Service, users UserResolver) Handler {
	return Handler{service: service, users: users}
}

func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/me", handler.me)
	router.Get("/admins", handler.admins)
	router.Post("/admins", handler.grant)
	router.Delete("/admins/{userID}", handler.revoke)
	router.Get("/organizations", handler.organizations)
	router.Get("/connections", handler.connections)
	router.Get("/queue", handler.queue)
	router.Get("/jobs", handler.jobs)
	router.Get("/dead-letters", handler.deadLetters)
	router.Post("/jobs/{jobID}/retry", handler.retryJob)
	router.Post("/jobs/{jobID}/discard", handler.discardJob)
	router.Post("/outbox/{eventID}/replay", handler.replayEvent)
	router.Post("/outbox/{eventID}/discard", handler.discardEvent)
	router.Post("/ai/jobs/{jobID}/retry", handler.retryAIJob)
	router.Post("/ai/jobs/{jobID}/discard", handler.discardAIJob)
	router.Post("/notifications/deliveries/{deliveryID}/discard", handler.discardDelivery)
	router.Get("/ai/nodes", handler.aiNodes)
	router.Get("/ai/runs", handler.aiRuns)
	router.Get("/ai/tenants/{tenantID}/conversations/{conversationID}/summary", handler.summary)
	router.Get("/usage", handler.usage)
	router.Get("/trace/tenants/{tenantID}/messages/{messageID}", handler.trace)
	return router
}

func (handler Handler) actor(w http.ResponseWriter, r *http.Request) (string, bool) {
	if handler.users == nil {
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", nil)
		return "", false
	}
	actorID, found, err := handler.users.User(r)
	if err != nil {
		httpplatform.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error", nil)
		return "", false
	}
	if !found || actorID == "" {
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", nil)
		return "", false
	}
	return actorID, true
}

func respond(w http.ResponseWriter, r *http.Request, status int, value any, err error) {
	switch {
	case err == nil:
		httpplatform.WriteJSON(w, status, value)
	case errors.Is(err, application.ErrForbidden):
		httpplatform.WriteError(w, r, http.StatusForbidden, "FORBIDDEN", "platform admin required", nil)
	case errors.Is(err, application.ErrNotFound):
		httpplatform.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "object not found", nil)
	case errors.Is(err, application.ErrConflict):
		httpplatform.WriteError(w, r, http.StatusConflict, "CONFLICT", "object state does not allow the command", nil)
	case errors.Is(err, application.ErrInvalid):
		httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request", nil)
	default:
		httpplatform.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error", nil)
	}
}

func limitFrom(r *http.Request) int {
	value, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil {
		return 0
	}
	return value
}

func (handler Handler) me(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	admin, err := handler.service.Me(r.Context(), actorID)
	respond(w, r, http.StatusOK, map[string]any{"userId": actorID, "platformAdmin": admin}, err)
}

func (handler Handler) admins(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	admins, err := handler.service.Admins(r.Context(), actorID)
	respond(w, r, http.StatusOK, map[string]any{"items": admins}, err)
}

func (handler Handler) grant(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Note  string `json:"note"`
	}
	if httpplatform.DecodeJSON(w, r, &body) != nil || strings.TrimSpace(body.Email) == "" {
		httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "email is required", nil)
		return
	}
	admin, created, err := handler.service.Grant(r.Context(), actorID, body.Email, body.Note)
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	respond(w, r, status, admin, err)
}

func (handler Handler) revoke(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	if _, err := handler.service.Revoke(r.Context(), actorID, chi.URLParam(r, "userID")); err != nil {
		respond(w, r, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (handler Handler) organizations(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	items, err := handler.service.Organizations(r.Context(), actorID)
	respond(w, r, http.StatusOK, map[string]any{"items": items}, err)
}

func (handler Handler) connections(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	items, err := handler.service.Connections(r.Context(), actorID)
	respond(w, r, http.StatusOK, map[string]any{"items": items}, err)
}

func (handler Handler) queue(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	stats, err := handler.service.Queue(r.Context(), actorID)
	respond(w, r, http.StatusOK, stats, err)
}

func (handler Handler) jobs(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	items, err := handler.service.Jobs(r.Context(), actorID, application.JobFilter{
		TenantID: query.Get("tenantId"), Status: query.Get("status"), Type: query.Get("type"), Limit: limitFrom(r),
	})
	respond(w, r, http.StatusOK, map[string]any{"items": items}, err)
}

func (handler Handler) deadLetters(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	letters, err := handler.service.DeadLetters(r.Context(), actorID, limitFrom(r))
	respond(w, r, http.StatusOK, letters, err)
}

func (handler Handler) retryJob(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	job, err := handler.service.RetryJob(r.Context(), actorID, chi.URLParam(r, "jobID"))
	respond(w, r, http.StatusOK, job, err)
}

func (handler Handler) discardJob(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	job, err := handler.service.DiscardJob(r.Context(), actorID, chi.URLParam(r, "jobID"))
	respond(w, r, http.StatusOK, job, err)
}

func (handler Handler) replayEvent(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	event, err := handler.service.ReplayEvent(r.Context(), actorID, chi.URLParam(r, "eventID"))
	respond(w, r, http.StatusOK, event, err)
}

func (handler Handler) discardEvent(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	event, err := handler.service.DiscardEvent(r.Context(), actorID, chi.URLParam(r, "eventID"))
	respond(w, r, http.StatusOK, event, err)
}

func (handler Handler) retryAIJob(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	job, err := handler.service.RetryAIJob(r.Context(), actorID, chi.URLParam(r, "jobID"))
	respond(w, r, http.StatusOK, job, err)
}

func (handler Handler) discardAIJob(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	job, err := handler.service.DiscardAIJob(r.Context(), actorID, chi.URLParam(r, "jobID"))
	respond(w, r, http.StatusOK, job, err)
}

func (handler Handler) discardDelivery(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	delivery, err := handler.service.DiscardDelivery(r.Context(), actorID, chi.URLParam(r, "deliveryID"))
	respond(w, r, http.StatusOK, delivery, err)
}

func (handler Handler) aiNodes(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	items, err := handler.service.AINodes(r.Context(), actorID)
	respond(w, r, http.StatusOK, map[string]any{"items": items}, err)
}

func (handler Handler) aiRuns(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	items, err := handler.service.AIRuns(r.Context(), actorID, application.RunFilter{
		TenantID: query.Get("tenantId"), Status: query.Get("status"), ApplicationStatus: query.Get("applicationStatus"), Limit: limitFrom(r),
	})
	respond(w, r, http.StatusOK, map[string]any{"items": items}, err)
}

func (handler Handler) summary(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	summary, err := handler.service.Summary(r.Context(), actorID, chi.URLParam(r, "tenantID"), chi.URLParam(r, "conversationID"))
	respond(w, r, http.StatusOK, summary, err)
}

func (handler Handler) usage(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	var from, to time.Time
	var err error
	if raw := r.URL.Query().Get("from"); raw != "" {
		if from, err = time.Parse(time.RFC3339, raw); err != nil {
			httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid from", nil)
			return
		}
	}
	if raw := r.URL.Query().Get("to"); raw != "" {
		if to, err = time.Parse(time.RFC3339, raw); err != nil {
			httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid to", nil)
			return
		}
	}
	report, err := handler.service.Usage(r.Context(), actorID, from, to)
	respond(w, r, http.StatusOK, report, err)
}

func (handler Handler) trace(w http.ResponseWriter, r *http.Request) {
	actorID, ok := handler.actor(w, r)
	if !ok {
		return
	}
	trace, err := handler.service.Trace(r.Context(), actorID, chi.URLParam(r, "tenantID"), chi.URLParam(r, "messageID"))
	respond(w, r, http.StatusOK, trace, err)
}
