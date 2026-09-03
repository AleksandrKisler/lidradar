package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"

	analyticsdomain "lidradar/backend/internal/analytics/domain"
	conversationapplication "lidradar/backend/internal/conversation/application"
	"lidradar/backend/internal/devdata"
	identitytransport "lidradar/backend/internal/identity/transport"
	riskapplication "lidradar/backend/internal/risk/application"
	"lidradar/backend/platform/config"
)

func TestFrontendDataThroughRealAPI(t *testing.T) {
	if err := devdata.ValidateTarget(config.EnvironmentTest, os.Getenv("LIDRADAR_DATABASE_URL")); err != nil {
		t.Skip("нужна отдельная база lidradar_frontend")
	}
	f := newAPIFixture(t)
	ctx := context.Background()
	password := "isolated-http-fixture-password"
	if _, err := devdata.Run(ctx, f.pool, config.EnvironmentTest, "up", password); err != nil {
		t.Fatal(err)
	}
	profiles := devdata.Profiles()
	cookies := make([]*http.Cookie, 3)
	for i, p := range profiles {
		credentials, _ := json.Marshal(map[string]string{"email": p.Email, "password": password})
		login := request(t, f.handler, http.MethodPost, "/api/v1/auth/login", string(credentials), nil, "")
		if login.Code != 200 {
			t.Fatalf("login %s: %d", p.Email, login.Code)
		}
		for _, cookie := range login.Result().Cookies() {
			if cookie.Name == identitytransport.SessionCookieName {
				cookies[i] = cookie
			}
		}
		if cookies[i] == nil {
			t.Fatal("session cookie missing")
		}
		me := request(t, f.handler, http.MethodGet, "/api/v1/auth/me", "", cookies[i], "")
		var identity struct {
			Memberships []struct{ TenantID, Role string }
		}
		if err := json.Unmarshal(me.Body.Bytes(), &identity); err != nil {
			t.Fatal(err)
		}
		if me.Code != 200 || len(identity.Memberships) != 1 || identity.Memberships[0].TenantID != p.TenantID || identity.Memberships[0].Role != "OWNER" {
			t.Fatalf("membership %s: %s", p.Email, me.Body.String())
		}
		for _, path := range []string{"/api/v1/organization", "/api/v1/locations", "/api/v1/services", "/api/v1/integrations", "/api/v1/notifications/preferences", "/api/v1/risks/precision", "/api/v1/organization/ml-consent", "/api/v1/revenue/confirmed-recovered?currency=RUB"} {
			response := request(t, f.handler, http.MethodGet, path, "", cookies[i], p.TenantID)
			if response.Code != 200 {
				t.Fatalf("%s %s: %d %s", p.Email, path, response.Code, response.Body.String())
			}
		}
		analytics := request(t, f.handler, http.MethodGet, "/api/v1/analytics/summary", "", cookies[i], p.TenantID)
		var summary analyticsdomain.Summary
		if err := json.Unmarshal(analytics.Body.Bytes(), &summary); err != nil {
			t.Fatal(err)
		}
		if analytics.Code != 200 || summary.Messages.Total != p.Conversations*6 || summary.Opportunities.Created != p.Conversations {
			t.Fatalf("analytics %s: %s", p.Email, analytics.Body.String())
		}
		wantRecovered := fmt.Sprintf("%d.00", p.Conversations/12*3500)
		if summary.Revenue.ConfirmedRecovered != wantRecovered {
			t.Fatalf("recovered %s: %s", p.Email, summary.Revenue.ConfirmedRecovered)
		}
		radarResponse := request(t, f.handler, http.MethodGet, "/api/v1/radar", "", cookies[i], p.TenantID)
		var radar riskapplication.Summary
		if err := json.Unmarshal(radarResponse.Body.Bytes(), &radar); err != nil {
			t.Fatal(err)
		}
		if radarResponse.Code != 200 || radar.OpenRisks != p.Conversations*5/12 || radar.ConfirmedRecoveredRevenue != wantRecovered {
			t.Fatalf("radar %s: %s", p.Email, radarResponse.Body.String())
		}
		// Пагинация: прочитать все страницы без повторяющихся переписок.
		seen := map[string]bool{}
		cursor := ""
		for pages := 0; pages < 20; pages++ {
			response := request(t, f.handler, http.MethodGet, "/api/v1/conversations?limit=50&cursor="+url.QueryEscape(cursor), "", cookies[i], p.TenantID)
			var page conversationapplication.ConversationPage
			if response.Code != 200 {
				t.Fatalf("conversations: %d", response.Code)
			}
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
				t.Fatal(err)
			}
			for _, conversation := range page.Items {
				if seen[conversation.ID] {
					t.Fatal("duplicate conversation on next page")
				}
				seen[conversation.ID] = true
				for _, suffix := range []string{"", "/messages"} {
					if r := request(t, f.handler, http.MethodGet, "/api/v1/conversations/"+conversation.ID+suffix, "", cookies[i], p.TenantID); r.Code != 200 {
						t.Fatalf("conversation detail: %d", r.Code)
					}
				}
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
		if len(seen) != p.Conversations {
			t.Fatalf("got %d conversations, expected %d", len(seen), p.Conversations)
		}
	}
	var riskID string
	if err := f.pool.QueryRow(ctx, `SELECT id FROM risk_signals WHERE tenant_id=$1 AND status='OPEN' ORDER BY id LIMIT 1`, profiles[1].TenantID).Scan(&riskID); err != nil {
		t.Fatal(err)
	}
	foreign := request(t, f.handler, http.MethodGet, "/api/v1/risks/"+riskID, "", cookies[0], profiles[0].TenantID)
	if foreign.Code != 404 {
		t.Fatalf("foreign entity disclosed: %d", foreign.Code)
	}
	foreign = request(t, f.handler, http.MethodGet, "/api/v1/risks", "", cookies[0], profiles[1].TenantID)
	if foreign.Code != 403 {
		t.Fatalf("foreign tenant allowed: %d", foreign.Code)
	}
	action := idempotentRequest(t, f.handler, http.MethodPost, "/api/v1/risks/"+riskID+"/actions", `{"type":"CALL","note":"Проверка ручной работы"}`, cookies[1], profiles[1].TenantID, "frontend-action-test")
	if action.Code != 201 {
		t.Fatalf("manual action: %d %s", action.Code, action.Body.String())
	}
	if _, err := devdata.Run(ctx, f.pool, config.EnvironmentTest, "down", ""); err != nil {
		t.Fatal(err)
	}
	for _, cookie := range cookies {
		if response := request(t, f.handler, http.MethodGet, "/api/v1/auth/me", "", cookie, ""); response.Code != 401 {
			t.Fatalf("old session survived rollback: %d", response.Code)
		}
	}
}
