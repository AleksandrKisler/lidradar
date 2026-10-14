package infrastructure_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/lifecycle/domain"
)

// Личная выгрузка на настоящей PostgreSQL (ADR 0059): данные ровно одного пользователя по всем его организациям,
// без секретов и без чужих данных.

func TestPersonalExportHoldsTheUsersOwnDataAcrossOrganizations(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	b := seedOrganization(t, pools.Owner, "b")
	// Менеджер организации a состоит и в организации b, где ничего не вносил.
	must(t, pools.Owner, `INSERT INTO memberships(id, tenant_id, user_id, role, status, created_at, updated_at) VALUES ($1, $2, $3, 'MANAGER', 'ACTIVE', $4, $4)`,
		newID(t), b.Tenant, a.Manager, seedAt.Add(time.Hour)) // позже, чтобы порядок списка не зависел от идентификаторов

	document, err := store.PersonalExport(context.Background(), a.Manager)
	if err != nil {
		t.Fatalf("PersonalExport() error = %v", err)
	}
	account := document.Account
	if account.ID != a.Manager || account.Email != a.ManagerEmail || account.DisplayName != "Менеджер a" || account.Status != "ACTIVE" ||
		account.EmailVerifiedAt == nil || account.DeletionDueAt != nil || !account.CreatedAt.Equal(seedAt) {
		t.Errorf("account = %+v", account)
	}
	if len(document.Memberships.Items) != 2 || document.Memberships.Truncated {
		t.Fatalf("memberships = %+v", document.Memberships)
	}
	if first, second := document.Memberships.Items[0], document.Memberships.Items[1]; first.Organization != a.Name || first.Role != "MANAGER" ||
		first.Status != "ACTIVE" || second.Organization != b.Name || first.RevokedAt != nil {
		t.Errorf("memberships = %+v", document.Memberships.Items)
	}
	if len(document.Sessions.Items) != 2 {
		t.Fatalf("sessions = %+v", document.Sessions)
	}
	var current, old *domain.ExportSession
	for index := range document.Sessions.Items {
		session := &document.Sessions.Items[index]
		if session.RevokedAt == nil {
			current = session
		} else {
			old = session
		}
	}
	if current == nil || old == nil || current.IPAddress != "203.0.113.12" || !strings.HasPrefix(current.UserAgent, "Agent/") || old.IPAddress != "203.0.113.13" {
		t.Errorf("sessions = %+v", document.Sessions.Items)
	}
	if len(document.AuthEvents.Items) != 2 || document.AuthEvents.Items[0].Operation != "PASSWORD_CHANGED" ||
		document.AuthEvents.Items[0].IPAddress != "203.0.113.13" || document.AuthEvents.Items[1].Operation != "USER_LOGGED_IN" {
		t.Errorf("auth events (newest first) = %+v", document.AuthEvents.Items)
	}
	if len(document.NotificationSettings.Items) != 1 {
		t.Fatalf("notification settings = %+v", document.NotificationSettings)
	}
	rule := document.NotificationSettings.Items[0]
	if rule.Organization != a.Name || rule.RiskType != "NO_RESPONSE" || !rule.InAppEnabled || !rule.TelegramEnabled || !rule.QuietHoursEnabled ||
		rule.QuietHoursStart == nil || *rule.QuietHoursStart != "22:00" || rule.QuietHoursEnd == nil || *rule.QuietHoursEnd != "08:00" ||
		rule.DigestTime == nil || *rule.DigestTime != "09:30" {
		t.Errorf("notification rule = %+v", rule)
	}
	if len(document.TelegramLinks.Items) != 1 || document.TelegramLinks.Items[0].Organization != a.Name || document.TelegramLinks.Items[0].TelegramUserID == 0 {
		t.Errorf("telegram links = %+v", document.TelegramLinks.Items)
	}
	if len(document.ConsentDecisions.Items) != 1 || document.ConsentDecisions.Items[0].Decision != "REVOKED" ||
		document.ConsentDecisions.Items[0].Scope != "DATASETS" || document.ConsentDecisions.Items[0].Organization != a.Name {
		t.Errorf("consent decisions = %+v", document.ConsentDecisions.Items)
	}
	if len(document.AuthoredRecords) != 2 {
		t.Fatalf("authored = %+v", document.AuthoredRecords)
	}
	if authored := document.AuthoredRecords[0]; authored.Organization != a.Name || authored.RiskFeedback != 1 || authored.StageChanges != 1 ||
		authored.Actions != 0 || authored.Outcomes != 0 || authored.RevenueConfirmations != 0 || authored.InvitationsCreated != 0 || authored.AuditEvents != 0 {
		t.Errorf("authored in %s = %+v", a.Name, authored)
	}
	if authored := document.AuthoredRecords[1]; authored != (domain.ExportAuthored{Organization: b.Name}) {
		t.Errorf("authored in %s must be all zeros: %+v", b.Name, authored)
	}
}

func TestPersonalExportCountsWhatTheOwnerWrote(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	document, err := store.PersonalExport(context.Background(), a.Owner)
	if err != nil {
		t.Fatal(err)
	}
	want := domain.ExportAuthored{
		Organization: a.Name, Actions: 1, Outcomes: 1, RevenueConfirmations: 1, InvitationsCreated: 2, AuditEvents: 1,
	}
	if len(document.AuthoredRecords) != 1 || document.AuthoredRecords[0] != want {
		t.Errorf("authored = %+v, want %+v", document.AuthoredRecords, want)
	}
	if len(document.ConsentDecisions.Items) != 1 || document.ConsentDecisions.Items[0].Decision != "GRANTED" {
		t.Errorf("consent decisions = %+v", document.ConsentDecisions.Items)
	}
}

func TestPersonalExportNeverContainsSecretsOrOtherPeoplesData(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	b := seedOrganization(t, pools.Owner, "b")
	document, err := store.PersonalExport(context.Background(), a.Manager)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	output := string(encoded)
	for _, secret := range append([]string{"password", "token_hash", "tokenHash", "tenant"}, a.Secrets...) {
		if strings.Contains(output, secret) {
			t.Errorf("в личной выгрузке секрет или служебное значение %q", secret)
		}
	}
	// Данные владельца той же организации и обеих организаций b не должны попасть к менеджеру a.
	for _, foreign := range []string{
		a.OwnerEmail, "Владелец a", "203.0.113.10", "203.0.113.11", b.Name, b.ManagerEmail, b.OwnerEmail, "203.0.113.20", "203.0.113.22",
		b.Marker, a.Marker, a.Owner, b.Owner, b.Manager, a.Tenant, b.Tenant,
	} {
		if strings.Contains(output, foreign) {
			t.Errorf("в личной выгрузке менеджера есть чужие данные: %q", foreign)
		}
	}
}

func TestPersonalExportShowsAScheduledDeletion(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	must(t, pools.Owner, `UPDATE users SET deletion_requested_at = $2, deletion_due_at = $3 WHERE id = $1`, a.Manager, seedAt, seedAt.Add(30*24*time.Hour))
	document, err := store.PersonalExport(context.Background(), a.Manager)
	if err != nil {
		t.Fatal(err)
	}
	if document.Account.DeletionRequestedAt == nil || document.Account.DeletionDueAt == nil || !document.Account.DeletionRequestedAt.Equal(seedAt) {
		t.Errorf("account = %+v", document.Account)
	}
}

func TestPersonalExportMarksATruncatedSection(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	// Ещё столько сессий, чтобы вместе с двумя засеянными их стало на одну больше предела.
	must(t, pools.Owner, `
		INSERT INTO sessions(id, user_id, token_hash, expires_at, created_at)
		SELECT gen_random_uuid(), $1::uuid, md5(n::text) || md5(n::text || 'x'), $2::timestamptz, $2::timestamptz - (n || ' seconds')::interval
		FROM generate_series(1, $3) AS n`, a.Manager, seedAt.Add(24*time.Hour), domain.SectionLimit-1)
	document, err := store.PersonalExport(context.Background(), a.Manager)
	if err != nil {
		t.Fatal(err)
	}
	if !document.Sessions.Truncated || len(document.Sessions.Items) != domain.SectionLimit {
		t.Fatalf("sessions: truncated=%v items=%d, want truncated at %d", document.Sessions.Truncated, len(document.Sessions.Items), domain.SectionLimit)
	}
	if document.AuthEvents.Truncated || document.Memberships.Truncated || document.NotificationSettings.Truncated {
		t.Errorf("обрезан не тот раздел: %+v", document)
	}
	// Ровно предел — это ещё не обрезка.
	must(t, pools.Owner, `DELETE FROM sessions WHERE user_id = $1 AND token_hash = (SELECT token_hash FROM sessions WHERE user_id = $1 ORDER BY created_at LIMIT 1)`, a.Manager)
	exact, err := store.PersonalExport(context.Background(), a.Manager)
	if err != nil {
		t.Fatal(err)
	}
	if exact.Sessions.Truncated || len(exact.Sessions.Items) != domain.SectionLimit {
		t.Errorf("при ровно %d строках раздел не должен считаться обрезанным: truncated=%v items=%d", domain.SectionLimit, exact.Sessions.Truncated, len(exact.Sessions.Items))
	}
}

func TestPersonalExportOfAnUnknownUserIsNotFound(t *testing.T) {
	store, pools := newStore(t)
	seedOrganization(t, pools.Owner, "a")
	if _, err := store.PersonalExport(context.Background(), newID(t)); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("PersonalExport() error = %v, want ErrNotFound", err)
	}
	if _, err := store.PersonalExport(context.Background(), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("PersonalExport() without a user error = %v, want ErrInvalid", err)
	}
}

func TestPersonalExportOfAUserWithoutOrganizationsIsEmptyButValid(t *testing.T) {
	store, pools := newStore(t)
	seedOrganization(t, pools.Owner, "a")
	lonely := seedUser(t, pools.Owner, "lonely@example.test", "Одинокий")
	document, err := store.PersonalExport(context.Background(), lonely)
	if err != nil {
		t.Fatal(err)
	}
	if document.Account.Email != "lonely@example.test" || len(document.Memberships.Items) != 0 || len(document.AuthoredRecords) != 0 {
		t.Errorf("document = %+v", document)
	}
	// Пустые списки кодируются как [], а не как null: читателю файла не нужно различать два вида пустоты.
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"items":null`) || strings.Contains(string(encoded), `"authoredRecords":null`) {
		t.Errorf("пустой список закодирован как null: %s", encoded)
	}
}
