package integration_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestMalformedPathIdentifiersAreBadRequests закрывает дефект жёсткого
// тестирования 2026-09-25: значение не в формате UUID в пути доходило до
// PostgreSQL и возвращалось как 500. Все маршруты с идентификатором в пути
// обязаны отвечать 400 INVALID_ARGUMENT до обращения к хранилищу — в том числе
// неаутентифицированный вебхук.
func TestMalformedPathIdentifiersAreBadRequests(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "malformed-ids-owner@example.com", "Владелец")
	tenantID := createOrganization(t, fixture, owner, "Организация с проверкой идентификаторов")

	const malformed = "not-a-uuid"
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		cookie bool
		tenant bool
	}{
		{"переписка", http.MethodGet, "/api/v1/conversations/" + malformed, "", true, true},
		{"сообщения переписки", http.MethodGet, "/api/v1/conversations/" + malformed + "/messages", "", true, true},
		{"изменение услуги", http.MethodPatch, "/api/v1/services/" + malformed, `{"active":false}`, true, true},
		{"отключение услуги", http.MethodDelete, "/api/v1/services/" + malformed, "", true, true},
		{"изменение точки", http.MethodPatch, "/api/v1/locations/" + malformed, `{"name":"Точка"}`, true, true},
		{"график точки", http.MethodPut, "/api/v1/locations/" + malformed + "/business-hours", `{"timezone":"Europe/Moscow","days":[]}`, true, true},
		{"здоровье подключения", http.MethodGet, "/api/v1/integrations/" + malformed + "/health", "", true, true},
		{"проверка подключения", http.MethodPost, "/api/v1/integrations/" + malformed + "/health/check", "", true, true},
		{"отключение подключения", http.MethodDelete, "/api/v1/integrations/" + malformed, "", true, true},
		{"вебхук: организация", http.MethodPost, "/api/v1/webhooks/GENERIC_WEBHOOK/" + malformed + "/" + malformed, `{"id":"1"}`, false, false},
		{"вебхук: подключение", http.MethodPost, "/api/v1/webhooks/GENERIC_WEBHOOK/" + tenantID + "/" + malformed, `{"id":"1"}`, false, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var cookie = owner.Cookie
			if !testCase.cookie {
				cookie = nil
			}
			tenant := ""
			if testCase.tenant {
				tenant = tenantID
			}
			response := request(t, fixture.handler, testCase.method, testCase.path, testCase.body, cookie, tenant)
			requireStatus(t, response, http.StatusBadRequest)
			var body struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "INVALID_ARGUMENT" {
				t.Fatalf("тело ответа = %s, err = %v", response.Body.String(), err)
			}
		})
	}

	// Корректный, но чужой или неизвестный идентификатор по-прежнему даёт 404.
	const unknown = "01990000-0000-7000-8000-00000000dead"
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/conversations/"+unknown+"/messages", "", owner.Cookie, tenantID), http.StatusNotFound)
	requireStatus(t, request(t, fixture.handler, http.MethodPatch, "/api/v1/services/"+unknown, `{"active":false}`, owner.Cookie, tenantID), http.StatusNotFound)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/integrations/"+unknown+"/health", "", owner.Cookie, tenantID), http.StatusNotFound)
	requireStatus(t, request(t, fixture.handler, http.MethodPost, "/api/v1/webhooks/GENERIC_WEBHOOK/"+tenantID+"/"+unknown, `{"id":"1"}`, nil, ""), http.StatusNotFound)
}
