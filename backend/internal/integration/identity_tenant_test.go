package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	catalogapplication "lidradar/backend/internal/catalog/application"
	cataloginfrastructure "lidradar/backend/internal/catalog/infrastructure"
	catalogtransport "lidradar/backend/internal/catalog/transport"
	connectorapplication "lidradar/backend/internal/connector/application"
	connectorinfrastructure "lidradar/backend/internal/connector/infrastructure"
	connectortransport "lidradar/backend/internal/connector/transport"
	conversationapplication "lidradar/backend/internal/conversation/application"
	conversationinfrastructure "lidradar/backend/internal/conversation/infrastructure"
	conversationtransport "lidradar/backend/internal/conversation/transport"
	eventsapplication "lidradar/backend/internal/events/application"
	eventsinfrastructure "lidradar/backend/internal/events/infrastructure"
	identityapplication "lidradar/backend/internal/identity/application"
	identityinfrastructure "lidradar/backend/internal/identity/infrastructure"
	identitytransport "lidradar/backend/internal/identity/transport"
	jobsapplication "lidradar/backend/internal/jobs/application"
	jobsinfrastructure "lidradar/backend/internal/jobs/infrastructure"
	opportunityapplication "lidradar/backend/internal/opportunity/application"
	opportunityinfrastructure "lidradar/backend/internal/opportunity/infrastructure"
	opportunitytransport "lidradar/backend/internal/opportunity/transport"
	riskapplication "lidradar/backend/internal/risk/application"
	riskdomain "lidradar/backend/internal/risk/domain"
	riskinfrastructure "lidradar/backend/internal/risk/infrastructure"
	risktransport "lidradar/backend/internal/risk/transport"
	"lidradar/backend/internal/tenant/application"
	"lidradar/backend/internal/tenant/domain"
	tenantinfrastructure "lidradar/backend/internal/tenant/infrastructure"
	tenanttransport "lidradar/backend/internal/tenant/transport"
	"lidradar/backend/internal/testsupport"
	cryptoplatform "lidradar/backend/platform/crypto"
	httpplatform "lidradar/backend/platform/http"
	"lidradar/backend/platform/ids"
	platformpostgres "lidradar/backend/platform/postgres"
)

type apiFixture struct {
	handler       http.Handler
	tenantService application.Service
	dispatcher    eventsapplication.Dispatcher
	worker        jobsapplication.Worker
	scheduler     jobsapplication.Scheduler
	candidates    opportunityapplication.CandidateProcessor
	pool          *pgxpool.Pool
}

func newAPIFixture(t *testing.T) apiFixture {
	t.Helper()
	pool := testsupport.Postgres(t)
	identityRepository := identityinfrastructure.NewPostgresRepository(pool)
	tenantRepository := tenantinfrastructure.NewPostgresRepository(pool)
	catalogRepository := cataloginfrastructure.NewPostgresRepository(pool)
	connectorRepository := connectorinfrastructure.NewPostgresRepository(pool)
	conversationRepository := conversationinfrastructure.NewPostgresRepository(pool)
	opportunityRepository := opportunityinfrastructure.NewPostgresRepository(pool)
	permissions := application.NewPermissionService(tenantRepository)
	tenantService := application.NewService(tenantRepository, permissions, ids.Generator{}, time.Now)
	catalogService := catalogapplication.NewService(catalogRepository, permissions, ids.Generator{}, time.Now)
	connectorService := connectorapplication.NewService(
		connectorRepository, permissions, connectorinfrastructure.NewRegistry(), ids.Generator{}, time.Now,
	)
	conversationService := conversationapplication.NewService(conversationRepository, permissions, ids.Generator{})
	opportunityService := opportunityapplication.NewService(opportunityRepository, permissions, ids.Generator{}, time.Now)
	candidateProcessor := opportunityapplication.NewCandidateProcessor(
		opportunityRepository, conversationService, catalogRepository, ids.Generator{}, time.Now,
	)
	normalization := connectorapplication.NewNormalizationService(
		connectorRepository, connectorinfrastructure.NewRegistry(), conversationService, time.Now,
	)
	jobStore := jobsinfrastructure.NewPostgresStore(pool)
	eventStore := eventsinfrastructure.NewPostgresStore(pool)
	riskRepository := riskinfrastructure.NewPostgresRepository(pool)
	riskStates := riskinfrastructure.NewPostgresStateReader(pool)
	riskPolicy := riskdomain.NoResponsePolicy{}
	riskEvents := risktransport.NewHub()
	riskEvaluator := riskapplication.NewEvaluator(
		riskRepository, riskStates, riskPolicy, ids.Generator{}, time.Now,
	).WithInvalidator(riskEvents)
	riskPlanner := riskapplication.NewPlanner(
		riskStates, riskStates, jobStore, riskEvaluator, riskPolicy, ids.Generator{}, time.Now,
	)
	dispatcher := eventsapplication.NewDispatcher(
		eventStore, "integration-outbox",
		map[string]eventsapplication.Handler{
			connectorapplication.NormalizationEventType: connectorapplication.NormalizationEventHandler(jobStore, ids.Generator{}),
			opportunityapplication.ConversationChangedEventType: eventsapplication.ChainHandlers(
				opportunityapplication.CandidateEventHandler(jobStore, ids.Generator{}),
				riskapplication.ConversationChangedEventHandler(jobStore, ids.Generator{}),
			),
			riskapplication.OpportunityCreatedEventType: riskapplication.OpportunityEventHandler(jobStore, ids.Generator{}),
			riskapplication.OpportunityStageEventType:   riskapplication.OpportunityEventHandler(jobStore, ids.Generator{}),
		}, time.Now, eventsapplication.DefaultLease,
	)
	worker := jobsapplication.NewWorker(
		jobStore, "integration-jobs",
		map[string]jobsapplication.Handler{
			connectorapplication.NormalizationJobType:   connectorapplication.NormalizationJobHandler(normalization),
			opportunityapplication.CandidateJobType:     opportunityapplication.CandidateJobHandler(candidateProcessor),
			riskapplication.RefreshJobType:              riskapplication.RefreshJobHandler(riskPlanner),
			riskapplication.NoResponseEvaluationJobType: riskapplication.EvaluationJobHandler(riskEvaluator),
		}, time.Now, jobsapplication.DefaultLease,
	)
	identityService := identityapplication.NewService(
		identityRepository, cryptoplatform.PasswordHasher{}, ids.Generator{}, identityinfrastructure.SessionTokens{}, time.Now, 24*time.Hour,
	)
	resolver := identitytransport.Resolver{Auth: identityService}
	router := httpplatform.NewRouter(
		"lidradar-api", slog.New(slog.NewTextHandler(io.Discard, nil)),
		platformpostgres.NewSchemaReadiness(pool),
	)
	router.Mount("/api/v1/auth", identitytransport.NewHandler(
		identityService, tenantService, identitytransport.CookieConfiguration{TTL: 24 * time.Hour},
	).Router())
	router.Mount("/api/v1/services", catalogtransport.NewHandler(catalogService, resolver).Router())
	router.Mount("/api/v1/conversations", conversationtransport.NewHandler(conversationService, resolver).Router())
	router.Mount("/api/v1/opportunities", opportunitytransport.NewHandler(opportunityService, resolver).Router())
	connectorHandler := connectortransport.NewHandler(connectorService, resolver)
	router.Mount("/api/v1/integrations", connectorHandler.ManagementRouter())
	router.Mount("/api/v1/webhooks", connectorHandler.WebhookRouter())
	router.Mount("/api/v1", tenanttransport.NewHandler(tenantService, resolver).Router())
	riskRadar := riskapplication.NewRadar(
		riskinfrastructure.NewPostgresRadarStore(pool), permissions, riskEvents, time.Now,
	)
	risktransport.NewHandler(riskRadar, resolver, riskEvents).RegisterRoutes(router, "/api/v1")
	return apiFixture{
		handler: router, tenantService: tenantService, dispatcher: dispatcher,
		worker: worker, candidates: candidateProcessor, pool: pool,
		scheduler: jobsapplication.NewScheduler(jobStore, time.Now),
	}
}

type registeredUser struct {
	ID     string
	Cookie *http.Cookie
}

func TestIdentityTenantOwnerFlowPermissionsAndIsolation(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "owner@example.com", "Owner")

	var plaintextMatches int
	if err := fixture.pool.QueryRow(context.Background(), `SELECT count(*) FROM sessions WHERE token_hash = $1`, owner.Cookie.Value).Scan(&plaintextMatches); err != nil {
		t.Fatal(err)
	}
	if plaintextMatches != 0 {
		t.Fatal("PostgreSQL contains a plaintext session token")
	}

	organizationResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/organizations", `{
		"name":"LidRadar Detailing","defaultTimezone":"Europe/Moscow","defaultCurrency":"RUB"
	}`, owner.Cookie, "")
	requireStatus(t, organizationResponse, http.StatusCreated)
	organizationID := jsonID(t, organizationResponse)

	locationResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Main studio","timezone":"Europe/Moscow","responseThresholdMinutes":45
	}`, owner.Cookie, organizationID)
	requireStatus(t, locationResponse, http.StatusCreated)
	locationID := jsonID(t, locationResponse)

	hoursResponse := request(t, fixture.handler, http.MethodPut, "/api/v1/locations/"+locationID+"/business-hours", `{
		"timezone":"Europe/Moscow","days":[
			{"weekday":1,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":2,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":3,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":4,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":5,"closed":false,"opensAt":"09:00","closesAt":"21:00"},
			{"weekday":6,"closed":false,"opensAt":"10:00","closesAt":"18:00"},
			{"weekday":7,"closed":true}
		]
	}`, owner.Cookie, organizationID)
	requireStatus(t, hoursResponse, http.StatusOK)
	if !strings.Contains(hoursResponse.Body.String(), `"weekday":7,"closed":true`) {
		t.Fatalf("business hours response = %s", hoursResponse.Body.String())
	}

	logoutResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/auth/logout", "", owner.Cookie, "")
	requireStatus(t, logoutResponse, http.StatusNoContent)
	if response := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", owner.Cookie, ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("logged out /me status = %d", response.Code)
	}
	owner.Cookie = login(t, fixture.handler, "owner@example.com")

	meResponse := request(t, fixture.handler, http.MethodGet, "/api/v1/auth/me", "", owner.Cookie, "")
	requireStatus(t, meResponse, http.StatusOK)
	if !strings.Contains(meResponse.Body.String(), organizationID) || !strings.Contains(meResponse.Body.String(), `"role":"OWNER"`) {
		t.Fatalf("/me response = %s", meResponse.Body.String())
	}
	organizationGet := request(t, fixture.handler, http.MethodGet, "/api/v1/organization", "", owner.Cookie, organizationID)
	requireStatus(t, organizationGet, http.StatusOK)
	locationsGet := request(t, fixture.handler, http.MethodGet, "/api/v1/locations", "", owner.Cookie, organizationID)
	requireStatus(t, locationsGet, http.StatusOK)
	if !strings.Contains(locationsGet.Body.String(), "Main studio") || !strings.Contains(locationsGet.Body.String(), `"businessHours":[`) {
		t.Fatalf("locations response after login = %s", locationsGet.Body.String())
	}

	manager := register(t, fixture.handler, "manager@example.com", "Manager")
	if _, err := fixture.tenantService.AddMember(context.Background(), owner.ID, organizationID, manager.ID, domain.RoleManager); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	managerUpdate := request(t, fixture.handler, http.MethodPatch, "/api/v1/organization", `{"name":"Unauthorized change"}`, manager.Cookie, organizationID)
	requireStatus(t, managerUpdate, http.StatusForbidden)

	ownerB := register(t, fixture.handler, "owner-b@example.com", "Owner B")
	organizationBResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/organizations", `{
		"name":"Tenant B private","defaultTimezone":"Europe/Moscow"
	}`, ownerB.Cookie, "")
	requireStatus(t, organizationBResponse, http.StatusCreated)
	organizationBID := jsonID(t, organizationBResponse)
	locationBResponse := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"Tenant B secret location","timezone":"Europe/Moscow"
	}`, ownerB.Cookie, organizationBID)
	requireStatus(t, locationBResponse, http.StatusCreated)
	locationBID := jsonID(t, locationBResponse)

	crossTenant := request(t, fixture.handler, http.MethodPatch, "/api/v1/locations/"+locationBID, `{"name":"Cross tenant"}`, owner.Cookie, organizationID)
	requireStatus(t, crossTenant, http.StatusNotFound)
	if strings.Contains(crossTenant.Body.String(), "Tenant B") {
		t.Fatalf("cross-tenant response disclosed data: %s", crossTenant.Body.String())
	}
	forbiddenTenant := request(t, fixture.handler, http.MethodGet, "/api/v1/locations", "", owner.Cookie, organizationBID)
	requireStatus(t, forbiddenTenant, http.StatusForbidden)
}

func register(t *testing.T, handler http.Handler, email, displayName string) registeredUser {
	t.Helper()
	response := request(t, handler, http.MethodPost, "/api/v1/auth/register", `{
		"email":"`+email+`","password":"very-secure-password","displayName":"`+displayName+`"
	}`, nil, "")
	requireStatus(t, response, http.StatusCreated)
	cookie := response.Result().Cookies()
	if len(cookie) != 1 || cookie[0].Name != identitytransport.SessionCookieName || !cookie[0].HttpOnly || cookie[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("registration cookie = %#v", cookie)
	}
	return registeredUser{ID: nestedUserID(t, response), Cookie: cookie[0]}
}

func login(t *testing.T, handler http.Handler, email string) *http.Cookie {
	t.Helper()
	response := request(t, handler, http.MethodPost, "/api/v1/auth/login", `{
		"email":"`+email+`","password":"very-secure-password"
	}`, nil, "")
	requireStatus(t, response, http.StatusOK)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %#v", cookies)
	}
	return cookies[0]
}

func request(t *testing.T, handler http.Handler, method, path, body string, cookie *http.Cookie, tenantID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "http://api.example"+path, bytes.NewBufferString(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	if tenantID != "" {
		request.Header.Set("X-Tenant-ID", tenantID)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func requireStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d; body = %s", response.Code, want, response.Body.String())
	}
}

func jsonID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var value struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil || value.ID == "" {
		t.Fatalf("decode ID response: %v; body = %s", err, response.Body.String())
	}
	return value.ID
}

func nestedUserID(t *testing.T, response *httptest.ResponseRecorder) string {
	t.Helper()
	var value struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil || value.User.ID == "" {
		t.Fatalf("decode user response: %v; body = %s", err, response.Body.String())
	}
	return value.User.ID
}
