package integration_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"lidradar/backend/internal/tenant/domain"
	"lidradar/backend/platform/tenantctx"
)

type radarItem struct {
	Risk struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Source string `json:"source"`
	} `json:"risk"`
	Opportunity *struct {
		ServiceID        *string `json:"serviceId"`
		PotentialRevenue *string `json:"potentialRevenue"`
		Currency         string  `json:"currency"`
	} `json:"opportunity"`
	Conversation *struct {
		ID          string `json:"id"`
		LastMessage *struct {
			Direction string  `json:"direction"`
			Preview   *string `json:"preview"`
		} `json:"lastMessage"`
	} `json:"conversation"`
	Contact *struct {
		DisplayName *string `json:"displayName"`
	} `json:"contact"`
	Service *struct {
		Name string `json:"name"`
	} `json:"service"`
	Channel *struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
	} `json:"channel"`
	ExternalLink struct {
		URL               *string `json:"url"`
		UnavailableReason *string `json:"unavailableReason"`
	} `json:"externalLink"`
	Recommendation *json.RawMessage  `json:"recommendation"`
	Actions        []json.RawMessage `json:"actions"`
	Outcome        *json.RawMessage  `json:"outcome"`
	Revenue        *json.RawMessage  `json:"revenue"`
}

// Обогащённые модели чтения Radar и переписок доступны менеджеру с
// risks.read/conversation.read, включая деньги сделки (ADR 0044,
// GAP-API-003/004/005/006/012/016, GAP-CONTRACT-002).
func TestFrontendReadModelsThroughAPI(t *testing.T) {
	fixture := newAPIFixture(t)
	owner := register(t, fixture.handler, "readiness-owner@example.com", "Владелец")
	tenantID := createOrganization(t, fixture, owner, "Организация интерфейса")
	manager := register(t, fixture.handler, "readiness-manager@example.com", "Менеджер")
	if _, err := fixture.tenantService.AddMember(tenantctx.WithTenant(context.Background(), tenantID), owner.ID, tenantID, manager.ID, domain.RoleManager); err != nil {
		t.Fatal(err)
	}
	scenario := provisionNoResponseRisk(t, fixture, owner, tenantID, "readiness")

	list := request(t, fixture.handler, http.MethodGet, "/api/v1/risks?active=true", "", manager.Cookie, tenantID)
	requireStatus(t, list, http.StatusOK)
	var page struct {
		Items      []radarItem `json:"items"`
		NextCursor *string     `json:"nextCursor"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.NextCursor != nil || page.Items[0].Risk.ID != scenario.riskID || page.Items[0].Risk.Status != "OPEN" {
		t.Fatalf("активная лента = %s", list.Body.String())
	}
	item := page.Items[0]
	if item.Opportunity == nil || item.Opportunity.PotentialRevenue == nil || *item.Opportunity.PotentialRevenue != "5000.00" ||
		item.Opportunity.ServiceID == nil || item.Service == nil || item.Service.Name != "Полировка" ||
		item.Contact == nil || item.Contact.DisplayName == nil || *item.Contact.DisplayName != "Ирина" ||
		item.Channel == nil || item.Channel.Provider != "GENERIC_WEBHOOK" || item.Channel.Name != "Канал readiness" ||
		item.Conversation == nil || item.Conversation.ID != scenario.conversationID || item.Conversation.LastMessage == nil ||
		item.Conversation.LastMessage.Preview == nil || *item.Conversation.LastMessage.Preview != "Нужна полировка" ||
		item.ExternalLink.URL != nil || item.ExternalLink.UnavailableReason == nil || *item.ExternalLink.UnavailableReason != "PROVIDER_UNSUPPORTED" ||
		item.Recommendation != nil || item.Outcome != nil || item.Revenue != nil || item.Actions == nil {
		t.Fatalf("контекст карточки для менеджера = %s", list.Body.String())
	}
	if !strings.Contains(list.Body.String(), `"recommendation":null`) || !strings.Contains(list.Body.String(), `"outcome":null`) ||
		!strings.Contains(list.Body.String(), `"revenue":null`) || !strings.Contains(list.Body.String(), `"actions":[]`) {
		t.Fatalf("необязательные связи должны быть явными null: %s", list.Body.String())
	}
	detail := request(t, fixture.handler, http.MethodGet, "/api/v1/risks/"+scenario.riskID, "", manager.Cookie, tenantID)
	requireStatus(t, detail, http.StatusOK)
	if !strings.Contains(detail.Body.String(), `"potentialRevenue":"5000.00"`) || !strings.Contains(detail.Body.String(), `"name":"Полировка"`) {
		t.Fatalf("карточка риска для менеджера = %s", detail.Body.String())
	}
	radar := request(t, fixture.handler, http.MethodGet, "/api/v1/radar?active=true", "", manager.Cookie, tenantID)
	requireStatus(t, radar, http.StatusOK)
	if !strings.Contains(radar.Body.String(), `"openRisks":1`) || !strings.Contains(radar.Body.String(), `"potentialRevenue":"5000.00"`) {
		t.Fatalf("сводка для менеджера = %s", radar.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/revenue/confirmed-recovered?currency=RUB", "", manager.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/analytics/summary", "", manager.Cookie, tenantID), http.StatusForbidden)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/services", "", manager.Cookie, tenantID), http.StatusForbidden)

	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/risks?status=OPEN&active=true", "", manager.Cookie, tenantID), http.StatusBadRequest)
	terminal := request(t, fixture.handler, http.MethodGet, "/api/v1/risks?active=false", "", manager.Cookie, tenantID)
	requireStatus(t, terminal, http.StatusOK)
	if !strings.Contains(terminal.Body.String(), `"items":[]`) {
		t.Fatalf("терминальные риски = %s", terminal.Body.String())
	}
	resolved := request(t, fixture.handler, http.MethodPost, "/api/v1/risks/"+scenario.riskID+"/resolve", "", manager.Cookie, tenantID)
	requireStatus(t, resolved, http.StatusOK)
	afterResolve := request(t, fixture.handler, http.MethodGet, "/api/v1/risks?status=RESOLVED,FALSE_POSITIVE", "", manager.Cookie, tenantID)
	requireStatus(t, afterResolve, http.StatusOK)
	if !strings.Contains(afterResolve.Body.String(), `"status":"RESOLVED"`) {
		t.Fatalf("фильтр по нескольким статусам = %s", afterResolve.Body.String())
	}
	activeAfter := request(t, fixture.handler, http.MethodGet, "/api/v1/risks?active=true", "", manager.Cookie, tenantID)
	requireStatus(t, activeAfter, http.StatusOK)
	if !strings.Contains(activeAfter.Body.String(), `"items":[]`) {
		t.Fatalf("активная лента после закрытия = %s", activeAfter.Body.String())
	}

	conversations := request(t, fixture.handler, http.MethodGet, "/api/v1/conversations?search=ирина", "", manager.Cookie, tenantID)
	requireStatus(t, conversations, http.StatusOK)
	if !strings.Contains(conversations.Body.String(), `"displayName":"Ирина"`) ||
		!strings.Contains(conversations.Body.String(), `"activeRisks":{"count":0,"maxSeverity":null}`) ||
		!strings.Contains(conversations.Body.String(), `"preview":"Нужна полировка"`) ||
		!strings.Contains(conversations.Body.String(), `"provider":"GENERIC_WEBHOOK"`) {
		t.Fatalf("список переписок = %s", conversations.Body.String())
	}
	withRisk := request(t, fixture.handler, http.MethodGet, "/api/v1/conversations?withRisk=true", "", manager.Cookie, tenantID)
	requireStatus(t, withRisk, http.StatusOK)
	if !strings.Contains(withRisk.Body.String(), `"items":[]`) {
		t.Fatalf("фильтр «С риском» после закрытия = %s", withRisk.Body.String())
	}
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/conversations?withRisk=maybe", "", manager.Cookie, tenantID), http.StatusBadRequest)
	requireStatus(t, request(t, fixture.handler, http.MethodGet, "/api/v1/conversations?locationId=not-a-uuid", "", manager.Cookie, tenantID), http.StatusBadRequest)
	missing := request(t, fixture.handler, http.MethodGet, "/api/v1/conversations?search=никого", "", manager.Cookie, tenantID)
	requireStatus(t, missing, http.StatusOK)
	if !strings.Contains(missing.Body.String(), `"items":[],"nextCursor":null`) {
		t.Fatalf("пустой поиск = %s", missing.Body.String())
	}
}
