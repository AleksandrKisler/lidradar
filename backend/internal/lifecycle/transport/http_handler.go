// Package transport публикует выгрузку данных организации через REST (ADR 0059). Личная выгрузка и закрытие учётной
// записи живут в транспорте модуля identity: они подтверждаются паролем и принадлежат учётной записи.
package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/lifecycle/application"
	"lidradar/backend/internal/lifecycle/domain"
	httpplatform "lidradar/backend/platform/http"
)

// writeTimeout — сколько клиенту дано на приём одного куска архива. Сервер считает WriteTimeout от начала запроса и
// оборвал бы долгую выгрузку; срок переносится перед каждой записью (как в потоке событий), поэтому живой клиент не
// прерывается, а клиент, перестав читать, отсекается через этот срок.
const writeTimeout = 30 * time.Second

// busyRetryAfter — через сколько повторить, если в процессе идёт предельное число выгрузок.
const busyRetryAfter = 30 * time.Second

type PrincipalResolver interface {
	Principal(*http.Request) (actorID, tenantID string, ok bool)
}

type Exporter interface {
	ExportOrganization(ctx context.Context, actorID, tenantID string, out io.Writer) error
}

type Handler struct {
	service    Exporter
	principals PrincipalResolver
	logger     *slog.Logger
	now        func() time.Time
}

func NewHandler(service Exporter, principals PrincipalResolver) Handler {
	return Handler{service: service, principals: principals, now: time.Now}
}

// WithLogger включает запись оборванных выгрузок в журнал (без содержимого).
func (handler Handler) WithLogger(logger *slog.Logger) Handler {
	handler.logger = logger
	return handler
}

// RegisterRoutes добавляет маршрут выгрузки к общему роутеру под префиксом версии API.
func (handler Handler) RegisterRoutes(router chi.Router, prefix string) {
	router.Post(prefix+"/organization/export", handler.exportOrganization)
}

// streamWriter отправляет заголовки при первой записи: всё, что может отказать до архива (право, предел, аудит,
// снимок), отвечает обычной ошибкой, а после первого байта статус изменить уже нельзя.
type streamWriter struct {
	w          http.ResponseWriter
	controller *http.ResponseController
	headers    func(http.Header)
	started    bool
}

func (stream *streamWriter) Write(data []byte) (int, error) {
	if !stream.started {
		stream.started = true
		stream.headers(stream.w.Header())
		stream.w.WriteHeader(http.StatusOK)
	}
	// Писатели без поддержки дедлайнов (тестовый recorder) возвращают ошибку, которую можно не учитывать.
	_ = stream.controller.SetWriteDeadline(time.Now().Add(writeTimeout))
	return stream.w.Write(data)
}

func (handler Handler) exportOrganization(w http.ResponseWriter, r *http.Request) {
	if handler.principals == nil || handler.service == nil {
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", nil)
		return
	}
	actorID, tenantID, ok := handler.principals.Principal(r)
	if !ok || actorID == "" || tenantID == "" {
		httpplatform.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "authentication required", nil)
		return
	}
	stream := &streamWriter{
		w: w, controller: http.NewResponseController(w),
		headers: func(header http.Header) {
			header.Set("Content-Type", "application/zip")
			header.Set("Content-Disposition", `attachment; filename="lidradar-organization-`+handler.now().UTC().Format("20060102")+`.zip"`)
			header.Set("Cache-Control", "no-store")
			header.Set("X-Accel-Buffering", "no")
		},
	}
	err := handler.service.ExportOrganization(r.Context(), actorID, tenantID, stream)
	if err == nil {
		return
	}
	if stream.started {
		// Статус уже отправлен. Нормальное завершение ответа выглядело бы как успешная загрузка, поэтому соединение
		// обрывается: клиент видит сбой загрузки, а архив без манифеста всё равно не откроется.
		if handler.logger != nil {
			handler.logger.Error("Выгрузка организации прервана", "event", "organization.export.aborted", "error", err,
				"request_id", httpplatform.RequestID(r.Context()))
		}
		panic(http.ErrAbortHandler)
	}
	handler.writeError(w, r, err)
}

func (handler Handler) writeError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, application.ErrForbidden):
		httpplatform.WriteError(w, r, http.StatusForbidden, "FORBIDDEN", "permission denied", nil)
	case errors.Is(err, application.ErrRateLimited):
		seconds := int64((application.RateLimitRetryAfter(err) + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		httpplatform.WriteError(w, r, http.StatusTooManyRequests, "RATE_LIMITED", "Too many requests", nil)
	case errors.Is(err, application.ErrBusy):
		w.Header().Set("Retry-After", strconv.FormatInt(int64(busyRetryAfter/time.Second), 10))
		httpplatform.WriteError(w, r, http.StatusTooManyRequests, "EXPORT_BUSY", "Another export is running, try again later", nil)
	case errors.Is(err, domain.ErrInvalid):
		httpplatform.WriteError(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid request", nil)
	case errors.Is(err, domain.ErrNotFound):
		httpplatform.WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "organization not found", nil)
	default:
		httpplatform.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error", nil)
	}
}
