// Package transport открывает операции выручки через версионированный API.
package transport

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/revenue/application"
	"lidradar/backend/internal/revenue/domain"
	httpplatform "lidradar/backend/platform/http"
)

type PrincipalResolver interface {
	Principal(*http.Request) (actorID, tenantID string, ok bool)
}

type Handler struct {
	service    application.Service
	principals PrincipalResolver
}

func NewHandler(service application.Service, principals PrincipalResolver) Handler {
	return Handler{service: service, principals: principals}
}

func (handler Handler) Router() http.Handler {
	router := chi.NewRouter()
	handler.RegisterRoutes(router, "")
	return router
}

func (handler Handler) RegisterRoutes(router chi.Router, prefix string) {
	prefix = strings.TrimSuffix(prefix, "/")
	router.Post(prefix+"/opportunities/{opportunityID}/revenue", handler.confirm)
	router.Get(prefix+"/revenue/confirmed-recovered", handler.total)
}

func (handler Handler) principal(w http.ResponseWriter, request *http.Request) (string, string, bool) {
	if handler.principals == nil {
		writeError(w, request, http.StatusUnauthorized, "UNAUTHENTICATED", "требуется аутентификация")
		return "", "", false
	}
	actor, tenant, ok := handler.principals.Principal(request)
	if !ok || actor == "" || tenant == "" {
		writeError(w, request, http.StatusUnauthorized, "UNAUTHENTICATED", "требуется аутентификация")
		return "", "", false
	}
	return actor, tenant, true
}

func decode(w http.ResponseWriter, request *http.Request, value any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "некорректный запрос")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "некорректный запрос")
		return false
	}
	return true
}

func (handler Handler) confirm(w http.ResponseWriter, request *http.Request) {
	actor, tenant, ok := handler.principal(w, request)
	if !ok {
		return
	}
	var body struct {
		Amount          string                 `json:"amount"`
		Currency        string                 `json:"currency"`
		AttributionType domain.AttributionType `json:"attributionType"`
		RiskID          string                 `json:"riskId"`
		ActionID        string                 `json:"actionId"`
		OutcomeID       string                 `json:"outcomeId"`
	}
	if !decode(w, request, &body) {
		return
	}
	confirmation, created, err := handler.service.Confirm(
		request.Context(), actor, tenant, chi.URLParam(request, "opportunityID"),
		strings.TrimSpace(request.Header.Get("Idempotency-Key")),
		application.ConfirmCommand{
			Amount: body.Amount, Currency: body.Currency, Type: body.AttributionType,
			RiskID: body.RiskID, ActionID: body.ActionID, OutcomeID: body.OutcomeID,
		},
	)
	if handle(w, request, err) {
		return
	}
	if created {
		writeJSON(w, http.StatusCreated, confirmation)
		return
	}
	writeJSON(w, http.StatusOK, confirmation)
}

func (handler Handler) total(w http.ResponseWriter, request *http.Request) {
	actor, tenant, ok := handler.principal(w, request)
	if !ok {
		return
	}
	currency := strings.ToUpper(strings.TrimSpace(request.URL.Query().Get("currency")))
	total, err := handler.service.ConfirmedRecovered(request.Context(), actor, tenant, currency)
	if handle(w, request, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"amount": total.String(), "currency": currency})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	httpplatform.WriteJSON(w, status, value)
}

func writeError(w http.ResponseWriter, request *http.Request, status int, code, message string) {
	httpplatform.WriteError(w, request, status, code, message, nil)
}

func handle(w http.ResponseWriter, request *http.Request, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, application.ErrForbidden):
		writeError(w, request, http.StatusForbidden, "FORBIDDEN", "недостаточно прав")
	case errors.Is(err, application.ErrNotFound):
		writeError(w, request, http.StatusNotFound, "NOT_FOUND", "объект не найден")
	case errors.Is(err, application.ErrConflict):
		writeError(w, request, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "ключ идемпотентности уже использован для другого запроса")
	case errors.Is(err, application.ErrInvalid), errors.Is(err, domain.ErrInvalid):
		writeError(w, request, http.StatusBadRequest, "INVALID_ARGUMENT", "некорректный запрос")
	default:
		writeError(w, request, http.StatusInternalServerError, "INTERNAL", "внутренняя ошибка")
	}
	return true
}
