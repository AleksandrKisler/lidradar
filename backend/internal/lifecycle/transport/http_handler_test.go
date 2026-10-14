package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"lidradar/backend/internal/lifecycle/application"
	"lidradar/backend/internal/lifecycle/domain"
	httpplatform "lidradar/backend/platform/http"
)

// Транспорт выгрузки организации (ADR 0059): маршрут, заголовки вложения, соответствие ошибок кодам до первого
// байта и обрыв после него, продление срока записи.

type fakeExporter struct {
	mu     sync.Mutex
	chunks [][]byte
	gap    time.Duration
	err    error
	actor  string
	tenant string
	// beforeError записывается до ошибки: ошибка после данных ведёт себя иначе, чем до них.
	calls int
}

func (f *fakeExporter) ExportOrganization(ctx context.Context, actorID, tenantID string, out io.Writer) error {
	f.mu.Lock()
	f.actor, f.tenant = actorID, tenantID
	f.calls++
	f.mu.Unlock()
	for _, chunk := range f.chunks {
		if _, err := out.Write(chunk); err != nil {
			return err
		}
		if f.gap > 0 {
			select {
			case <-time.After(f.gap):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return f.err
}

type fakePrincipals struct {
	actor, tenant string
	ok            bool
}

func (p fakePrincipals) Principal(*http.Request) (string, string, bool) {
	return p.actor, p.tenant, p.ok
}

func newRouter(exporter Exporter, principals PrincipalResolver, logger *slog.Logger) http.Handler {
	router := httpplatform.NewRouter("test", logger, nil)
	handler := NewHandler(exporter, principals)
	handler.now = func() time.Time { return time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC) }
	if logger != nil {
		handler = handler.WithLogger(logger)
	}
	handler.RegisterRoutes(router, "/api/v1")
	return router
}

func post(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/organization/export", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func errorCode(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("ответ не разобрался: %v: %s", err, response.Body.String())
	}
	return payload.Error.Code
}

func TestExportIsAZipAttachmentThatIsNeverCached(t *testing.T) {
	exporter := &fakeExporter{chunks: [][]byte{[]byte("PK-one"), []byte("PK-two")}}
	response := post(t, newRouter(exporter, fakePrincipals{actor: "actor-1", tenant: "tenant-1", ok: true}, nil))
	if response.Code != http.StatusOK || response.Body.String() != "PK-onePK-two" {
		t.Fatalf("%d %q", response.Code, response.Body.String())
	}
	header := response.Header()
	if header.Get("Content-Type") != "application/zip" ||
		header.Get("Content-Disposition") != `attachment; filename="lidradar-organization-20261014.zip"` ||
		header.Get("Cache-Control") != "no-store" || header.Get("X-Accel-Buffering") != "no" {
		t.Errorf("заголовки: %v", header)
	}
	if exporter.actor != "actor-1" || exporter.tenant != "tenant-1" {
		t.Errorf("служба получила %q / %q", exporter.actor, exporter.tenant)
	}
}

// Общий middleware тоже ставит Cache-Control, поэтому заголовок вложения проверяется на голом маршрутизаторе: выгрузка
// не должна зависеть от того, кто стоит перед обработчиком.
func TestExportSetsItsOwnHeadersWithoutTheSharedMiddleware(t *testing.T) {
	router := chi.NewRouter()
	handler := NewHandler(&fakeExporter{chunks: [][]byte{[]byte("PK")}}, fakePrincipals{actor: "actor-1", tenant: "tenant-1", ok: true})
	handler.now = func() time.Time { return time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC) }
	handler.RegisterRoutes(router, "/api/v1")
	response := post(t, router)
	header := response.Header()
	if header.Get("Cache-Control") != "no-store" || header.Get("Content-Type") != "application/zip" ||
		header.Get("X-Accel-Buffering") != "no" ||
		header.Get("Content-Disposition") != `attachment; filename="lidradar-organization-20261014.zip"` {
		t.Errorf("заголовки обработчика: %v", header)
	}
}

// countingWriter считает вызовы WriteHeader: настоящий сервер на каждый лишний вызов пишет предупреждение, а
// заголовки после первого куска уже не меняются.
type countingWriter struct {
	header      http.Header
	headerCalls int
	statuses    []int
	body        bytes.Buffer
}

func (w *countingWriter) Header() http.Header { return w.header }
func (w *countingWriter) WriteHeader(status int) {
	w.headerCalls++
	w.statuses = append(w.statuses, status)
}
func (w *countingWriter) Write(data []byte) (int, error) { return w.body.Write(data) }

func TestExportSendsTheStatusOnceForAStreamOfManyChunks(t *testing.T) {
	router := chi.NewRouter()
	handler := NewHandler(&fakeExporter{chunks: [][]byte{[]byte("a"), []byte("b"), []byte("c")}}, fakePrincipals{actor: "actor-1", tenant: "tenant-1", ok: true})
	handler.RegisterRoutes(router, "/api/v1")
	writer := &countingWriter{header: http.Header{}}
	router.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/api/v1/organization/export", nil))
	if writer.headerCalls != 1 || writer.statuses[0] != http.StatusOK || writer.body.String() != "abc" {
		t.Errorf("WriteHeader вызван %d раз %v, тело %q", writer.headerCalls, writer.statuses, writer.body.String())
	}
}

func TestExportRequiresASessionAndAnOrganization(t *testing.T) {
	exporter := &fakeExporter{}
	for name, principals := range map[string]PrincipalResolver{
		"нет сессии":         fakePrincipals{},
		"пустой актор":       fakePrincipals{tenant: "tenant-1", ok: true},
		"пустая организация": fakePrincipals{actor: "actor-1", ok: true},
	} {
		response := post(t, newRouter(exporter, principals, nil))
		if response.Code != http.StatusUnauthorized || errorCode(t, response) != "UNAUTHENTICATED" {
			t.Errorf("%s: %d %s", name, response.Code, response.Body.String())
		}
		if response.Header().Get("Content-Disposition") != "" {
			t.Errorf("%s: отказ выглядит как вложение", name)
		}
	}
	if exporter.calls != 0 {
		t.Errorf("служба вызвана %d раз без сессии", exporter.calls)
	}
	if response := post(t, newRouter(nil, fakePrincipals{ok: true}, nil)); response.Code != http.StatusUnauthorized {
		t.Errorf("без службы: %d", response.Code)
	}
}

func TestExportErrorsBeforeTheFirstByteBecomeStableResponses(t *testing.T) {
	for name, tc := range map[string]struct {
		err        error
		status     int
		code       string
		retryAfter string
	}{
		"нет права":                       {application.ErrForbidden, http.StatusForbidden, "FORBIDDEN", ""},
		"предел организации":              {application.RateLimitError{RetryAfter: 41*time.Minute + time.Second}, http.StatusTooManyRequests, "RATE_LIMITED", "2461"},
		"процесс занят":                   {application.ErrBusy, http.StatusTooManyRequests, "EXPORT_BUSY", "30"},
		"неверный запрос":                 {domain.ErrInvalid, http.StatusBadRequest, "INVALID_ARGUMENT", ""},
		"нет организации":                 {domain.ErrNotFound, http.StatusNotFound, "NOT_FOUND", ""},
		"сбой базы не раскрывает деталей": {errors.New("connection refused to 10.0.0.5"), http.StatusInternalServerError, "INTERNAL_ERROR", ""},
	} {
		response := post(t, newRouter(&fakeExporter{err: tc.err}, fakePrincipals{actor: "a", tenant: "t", ok: true}, nil))
		if response.Code != tc.status || errorCode(t, response) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", name, response.Code, response.Body.String(), tc.status, tc.code)
		}
		if got := response.Header().Get("Retry-After"); got != tc.retryAfter {
			t.Errorf("%s: Retry-After = %q, want %q", name, got, tc.retryAfter)
		}
		if strings.Contains(response.Body.String(), "10.0.0.5") {
			t.Errorf("%s: внутренняя ошибка попала в ответ: %s", name, response.Body.String())
		}
		if response.Header().Get("Content-Disposition") != "" || response.Header().Get("Content-Type") != "application/json" {
			t.Errorf("%s: ошибка до первого байта должна быть обычным JSON-ответом: %v", name, response.Header())
		}
	}
}

func TestExportAcceptsOnlyPost(t *testing.T) {
	handler := newRouter(&fakeExporter{}, fakePrincipals{actor: "a", tenant: "t", ok: true}, nil)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, "/api/v1/organization/export", nil))
		if response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusNotFound {
			t.Errorf("%s: %d", method, response.Code)
		}
	}
}

// Ошибка после первого байта: статус уже отправлен, поэтому ответ обрывается. Нормальное завершение выглядело бы как
// успешная загрузка оборванного архива.
func TestExportAbortsTheConnectionWhenItBreaksMidway(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	exporter := &fakeExporter{chunks: [][]byte{bytes.Repeat([]byte("A"), 8192)}, err: errors.New("database connection lost")}
	server := httptest.NewServer(newRouter(exporter, fakePrincipals{actor: "a", tenant: "t", ok: true}, logger))
	defer server.Close()

	response, err := http.Post(server.URL+"/api/v1/organization/export", "application/json", nil)
	if err == nil {
		defer response.Body.Close()
		_, err = io.ReadAll(response.Body)
	}
	if err == nil {
		t.Fatal("клиент увидел успешное завершение оборванной выгрузки")
	}
	if !strings.Contains(logs.String(), "organization.export.aborted") {
		t.Errorf("обрыв не записан в журнал: %s", logs.String())
	}
	if strings.Contains(logs.String(), "database connection lost") && strings.Contains(logs.String(), "http.panic") {
		t.Errorf("осознанный обрыв записан как паника: %s", logs.String())
	}
}

// Сервер считает WriteTimeout от начала запроса. Выгрузка живёт дольше, поэтому срок записи переносится перед каждым
// куском; живой клиент получает архив целиком.
func TestExportOutlivesTheServerWriteTimeout(t *testing.T) {
	chunks := make([][]byte, 6)
	for index := range chunks {
		chunks[index] = bytes.Repeat([]byte{byte('a' + index)}, 16384)
	}
	exporter := &fakeExporter{chunks: chunks, gap: 120 * time.Millisecond}
	server := httptest.NewUnstartedServer(newRouter(exporter, fakePrincipals{actor: "a", tenant: "t", ok: true}, nil))
	server.Config.WriteTimeout = 200 * time.Millisecond
	server.Start()
	defer server.Close()

	started := time.Now()
	response, err := http.Post(server.URL+"/api/v1/organization/export", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("долгая выгрузка оборвана сервером: %v", err)
	}
	if want := 6 * 16384; len(body) != want {
		t.Fatalf("получено %d байт, ожидалось %d", len(body), want)
	}
	if elapsed := time.Since(started); elapsed < 600*time.Millisecond {
		t.Fatalf("выгрузка шла %s: тест не проверяет выход за WriteTimeout", elapsed)
	}
}

func TestExportStopsWhenTheClientCancels(t *testing.T) {
	exporter := &fakeExporter{chunks: [][]byte{bytes.Repeat([]byte("A"), 8192), bytes.Repeat([]byte("B"), 8192)}, gap: 2 * time.Second}
	server := httptest.NewServer(newRouter(exporter, fakePrincipals{actor: "a", tenant: "t", ok: true}, nil))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/organization/export", nil)
	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		defer response.Body.Close()
		_, err = io.ReadAll(response.Body)
	}
	if err == nil {
		t.Fatal("отмена клиента не прервала загрузку")
	}
	if time.Since(started) > 1500*time.Millisecond {
		t.Errorf("загрузка не прервалась вовремя: %s", time.Since(started))
	}
}
