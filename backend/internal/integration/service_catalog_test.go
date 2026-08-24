package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lidradar/backend/internal/tenant/domain"
)

func TestServiceCatalogOwnerCRUDMoneyPermissionsAndIsolation(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "catalog-owner@example.com", "Catalog Owner")
	tenantID := createOrganization(t, fixture, owner, "Catalog tenant")
	locationID := createLocation(t, fixture, owner, tenantID, "Catalog location")

	withoutPrices := request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{
		"name":"  Consultation  ","currency":"RUB"
	}`, owner.Cookie, tenantID)
	requireStatus(t, withoutPrices, http.StatusCreated)
	withoutPricesID := jsonID(t, withoutPrices)
	if !strings.Contains(withoutPrices.Body.String(), `"priceFrom":null`) || !strings.Contains(withoutPrices.Body.String(), `"priceTo":null`) {
		t.Fatalf("service without prices = %s", withoutPrices.Body.String())
	}
	var priceFromIsNull, priceToIsNull bool
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT price_from IS NULL, price_to IS NULL FROM service_catalog_items
		WHERE tenant_id = $1 AND id = $2`, tenantID, withoutPricesID,
	).Scan(&priceFromIsNull, &priceToIsNull); err != nil || !priceFromIsNull || !priceToIsNull {
		t.Fatalf("missing prices were not preserved as SQL NULL: %v, %v, %v", priceFromIsNull, priceToIsNull, err)
	}

	created := request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{
		"name":"  Ceramic   Coating ","locationId":"`+locationID+`",
		"priceFrom":"1200","priceTo":"1499.9","currency":"rub"
	}`, owner.Cookie, tenantID)
	requireStatus(t, created, http.StatusCreated)
	serviceID := jsonID(t, created)
	if !strings.Contains(created.Body.String(), `"normalizedName":"ceramic coating"`) ||
		!strings.Contains(created.Body.String(), `"priceFrom":"1200.00"`) ||
		!strings.Contains(created.Body.String(), `"priceTo":"1499.90"`) {
		t.Fatalf("created service = %s", created.Body.String())
	}
	var storedFrom, storedTo string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT price_from::text, price_to::text FROM service_catalog_items
		WHERE tenant_id = $1 AND id = $2`, tenantID, serviceID,
	).Scan(&storedFrom, &storedTo); err != nil || storedFrom != "1200.00" || storedTo != "1499.90" {
		t.Fatalf("stored exact prices = %q, %q, %v", storedFrom, storedTo, err)
	}

	invalidCases := []string{
		`{"name":"Negative","priceFrom":"-0.01"}`,
		`{"name":"Reversed","priceFrom":"2.00","priceTo":"1.99"}`,
		`{"name":"JSON number","priceFrom":12.50}`,
		`{"name":"Too precise","priceFrom":"1.001"}`,
	}
	for _, body := range invalidCases {
		response := request(t, fixture.handler, http.MethodPost, "/api/v1/services", body, owner.Cookie, tenantID)
		requireStatus(t, response, http.StatusBadRequest)
	}

	updated := request(t, fixture.handler, http.MethodPatch, "/api/v1/services/"+serviceID, `{
		"name":"Premium Coating","locationId":null,"priceFrom":null,"priceTo":null
	}`, owner.Cookie, tenantID)
	requireStatus(t, updated, http.StatusOK)
	if !strings.Contains(updated.Body.String(), `"locationId":null`) ||
		!strings.Contains(updated.Body.String(), `"priceFrom":null`) ||
		!strings.Contains(updated.Body.String(), `"normalizedName":"premium coating"`) {
		t.Fatalf("updated service = %s", updated.Body.String())
	}

	manager := register(t, fixture.handler, "catalog-manager@example.com", "Catalog Manager")
	if _, err := fixture.tenantService.AddMember(context.Background(), owner.ID, tenantID, manager.ID, domain.RoleManager); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{"name":"Forbidden"}`, manager.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/services", "", manager.Cookie, tenantID), http.StatusForbidden)

	ownerB := register(t, fixture.handler, "catalog-owner-b@example.com", "Catalog Owner B")
	tenantBID := createOrganization(t, fixture, ownerB, "Catalog tenant B")
	serviceB := request(t, fixture.handler, http.MethodPost, "/api/v1/services", `{"name":"Tenant B secret","priceFrom":"900.00"}`, ownerB.Cookie, tenantBID)
	requireStatus(t, serviceB, http.StatusCreated)
	serviceBID := jsonID(t, serviceB)

	crossUpdate := request(t, fixture.handler, http.MethodPatch, "/api/v1/services/"+serviceBID, `{"name":"Cross tenant"}`, owner.Cookie, tenantID)
	requireStatus(t, crossUpdate, http.StatusNotFound)
	if strings.Contains(crossUpdate.Body.String(), "Tenant B") {
		t.Fatalf("cross-tenant response disclosed data: %s", crossUpdate.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/services/"+serviceBID, "", owner.Cookie, tenantID), http.StatusNotFound)

	listed := request(t, fixture.handler, http.MethodGet, "/api/v1/services", "", owner.Cookie, tenantID)
	requireStatus(t, listed, http.StatusOK)
	if strings.Contains(listed.Body.String(), "Tenant B secret") {
		t.Fatalf("tenant list disclosed data: %s", listed.Body.String())
	}

	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/services/"+serviceID, "", owner.Cookie, tenantID), http.StatusNoContent)
	requireStatus(t, request(t, fixture.handler, http.MethodDelete, "/api/v1/services/"+serviceID, "", owner.Cookie, tenantID), http.StatusNoContent)
	listed = request(t, fixture.handler, http.MethodGet, "/api/v1/services", "", owner.Cookie, tenantID)
	requireStatus(t, listed, http.StatusOK)
	if !serviceIsInactive(t, listed.Body.Bytes(), serviceID) {
		t.Fatalf("deactivated service missing or active: %s", listed.Body.String())
	}
}

func createOrganization(t *testing.T, fixture apiFixture, owner registeredUser, name string) string {
	t.Helper()
	response := request(t, fixture.handler, http.MethodPost, "/api/v1/organizations", `{
		"name":"`+name+`","defaultTimezone":"Europe/Moscow","defaultCurrency":"RUB"
	}`, owner.Cookie, "")
	requireStatus(t, response, http.StatusCreated)
	return jsonID(t, response)
}

func createLocation(t *testing.T, fixture apiFixture, owner registeredUser, tenantID, name string) string {
	t.Helper()
	response := request(t, fixture.handler, http.MethodPost, "/api/v1/locations", `{
		"name":"`+name+`","timezone":"Europe/Moscow"
	}`, owner.Cookie, tenantID)
	requireStatus(t, response, http.StatusCreated)
	return jsonID(t, response)
}

func serviceIsInactive(t *testing.T, body []byte, serviceID string) bool {
	t.Helper()
	var response struct {
		Items []struct {
			ID     string `json:"id"`
			Active bool   `json:"active"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode service list: %v", err)
	}
	for _, item := range response.Items {
		if item.ID == serviceID {
			return !item.Active
		}
	}
	return false
}
