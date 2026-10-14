package infrastructure_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/lifecycle/application"
	"lidradar/backend/internal/lifecycle/domain"
	"lidradar/backend/internal/lifecycle/infrastructure"
	"lidradar/backend/internal/testsupport"
)

// Снимок организации для выгрузки на настоящей PostgreSQL (ADR 0059): только своя организация, ни одного секрета,
// порядок полей из реестра, один согласованный момент.

func newStore(t *testing.T) (*infrastructure.PostgresStore, testsupport.Pools) {
	t.Helper()
	pools := testsupport.PostgresRoles(t)
	return infrastructure.NewPostgresStore(pools.Platform, pools.App), pools
}

// collect читает все наборы снимка: файл → строки.
func collect(t *testing.T, snapshot application.Snapshot) map[string][]string {
	t.Helper()
	result := map[string][]string{}
	for _, dataset := range domain.OrganizationDatasets {
		lines := []string{}
		if err := snapshot.Rows(context.Background(), dataset, func(line []byte) error {
			lines = append(lines, string(line))
			return nil
		}); err != nil {
			t.Fatalf("набор %s: %v", dataset.File, err)
		}
		result[dataset.File] = lines
	}
	return result
}

func jsonKeys(t *testing.T, line string) []string {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(line))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		t.Fatalf("строка не объект: %q (%v)", line, err)
	}
	var keys []string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key.(string))
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			t.Fatal(err)
		}
	}
	return keys
}

func openSnapshot(t *testing.T, store *infrastructure.PostgresStore, tenant string) application.Snapshot {
	t.Helper()
	snapshot, err := store.OpenSnapshot(context.Background(), tenant)
	if err != nil {
		t.Fatalf("OpenSnapshot() error = %v", err)
	}
	t.Cleanup(func() { _ = snapshot.Close(context.Background()) })
	return snapshot
}

func TestSnapshotReadsEveryDatasetOfTheRequestedOrganizationOnly(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	b := seedOrganization(t, pools.Owner, "b")

	files := collect(t, openSnapshot(t, store, a.Tenant))
	var everything strings.Builder
	for _, dataset := range domain.OrganizationDatasets {
		lines := files[dataset.File]
		if len(lines) == 0 {
			t.Errorf("набор %s пуст: сид содержит строку в каждой таблице", dataset.File)
		}
		for _, line := range lines {
			everything.WriteString(line + "\n")
		}
	}
	output := everything.String()
	if !strings.Contains(output, a.Marker) {
		t.Errorf("содержимое организации a не попало в выгрузку: нет %s", a.Marker)
	}
	for _, foreign := range []string{
		b.Marker, b.Tenant, b.Owner, b.Manager, b.Conversation, b.Incoming, b.Contact, b.Risk, b.OwnerEmail, b.ManagerEmail, b.Name,
	} {
		if strings.Contains(output, foreign) {
			t.Fatalf("в выгрузке организации a есть данные организации b: %q", foreign)
		}
	}
	for file, lines := range files {
		for _, line := range lines {
			if strings.Contains(line, `"tenant_id"`) || strings.Contains(line, `"tenantId"`) {
				t.Fatalf("%s: идентификатор организации не должен выходить наружу: %s", file, line)
			}
		}
	}
	// Каждая организация видна только своему снимку и в обратную сторону.
	other := collect(t, openSnapshot(t, store, b.Tenant))
	var otherOutput strings.Builder
	for _, lines := range other {
		otherOutput.WriteString(strings.Join(lines, "\n"))
	}
	if strings.Contains(otherOutput.String(), a.Marker) || !strings.Contains(otherOutput.String(), b.Marker) {
		t.Errorf("снимок организации b смешан с организацией a")
	}
}

func TestSnapshotNeverContainsSecretsOrServiceValues(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	snapshot := openSnapshot(t, store, a.Tenant)
	organization, err := snapshot.Organization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var everything strings.Builder
	everything.Write(organization)
	for _, lines := range collect(t, snapshot) {
		everything.WriteString(strings.Join(lines, "\n"))
	}
	output := everything.String()
	for _, secret := range a.Secrets {
		if strings.Contains(output, secret) {
			t.Errorf("в выгрузке секрет или служебное значение %q", secret)
		}
	}
	// Колонки реестра не должны называть секреты.
	for _, forbidden := range []string{"encrypted_credentials", "verification_secret_hash", "code_hash", "password_hash", "token_hash", "object_key"} {
		if strings.Contains(output, forbidden) {
			t.Errorf("в выгрузке есть поле %s", forbidden)
		}
	}
}

func TestSnapshotKeepsTheRegistryColumnOrder(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	files := collect(t, openSnapshot(t, store, a.Tenant))
	for _, dataset := range domain.OrganizationDatasets {
		for _, line := range files[dataset.File] {
			got := strings.Join(jsonKeys(t, line), ",")
			if want := strings.Join(dataset.Columns, ","); got != want {
				t.Errorf("%s: поля %s, ожидалось по реестру %s", dataset.File, got, want)
			}
		}
	}
}

func TestSnapshotOrganizationDocumentHasNoIdentifier(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	document, err := openSnapshot(t, store, a.Tenant).Organization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(document, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["name"] != a.Name || fields["default_currency"] != "RUB" || fields["default_timezone"] != "Europe/Moscow" || fields["status"] != "ACTIVE" {
		t.Errorf("organization.json = %v", fields)
	}
	if _, leaked := fields["id"]; leaked || strings.Contains(string(document), a.Tenant) {
		t.Errorf("организация не должна выдавать свой идентификатор: %s", document)
	}
	if got := strings.Join(jsonKeys(t, string(document)), ","); got != strings.Join(domain.OrganizationColumns, ",") {
		t.Errorf("поля organization.json = %s", got)
	}
}

func TestSnapshotTimesAreUTC(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	// Сеанс владельца схемы в другом часовом поясе не должен менять представление времён в файлах.
	if _, err := pools.App.Exec(context.Background(), `SET TIME ZONE 'Asia/Vladivostok'`); err != nil {
		t.Fatal(err)
	}
	snapshot := openSnapshot(t, store, a.Tenant)
	for _, line := range collect(t, snapshot)["messages"] {
		var message struct {
			SentAt string `json:"sent_at"`
		}
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(message.SentAt, "2026-10-01T09:00:00") || !strings.HasSuffix(message.SentAt, "+00:00") {
			t.Errorf("sent_at = %q, ожидалось время в UTC", message.SentAt)
		}
	}
	if !snapshot.At().Equal(snapshot.At().UTC()) || time.Since(snapshot.At()) > time.Minute {
		t.Errorf("момент снимка = %s", snapshot.At())
	}
}

func TestSnapshotIsOneConsistentMoment(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	snapshot := openSnapshot(t, store, a.Tenant)
	before := len(collect(t, snapshot)["messages"])

	// Другая транзакция пишет и фиксирует новое сообщение после открытия снимка.
	must(t, pools.Owner, `INSERT INTO messages(id, tenant_id, conversation_id, connection_id, external_id, direction, type, text, sent_at, received_at, metadata, created_at)
		VALUES ($1, $2, $3, $4, 'late-message', 'INCOMING', 'TEXT', 'позднее сообщение', $5, $5, '{}'::jsonb, $5)`,
		newID(t), a.Tenant, a.Conversation, a.Channel, seedAt)
	must(t, pools.Owner, `UPDATE contacts SET display_name = 'Изменённый' WHERE id = $1`, a.Contact)

	again := collect(t, snapshot)
	if len(again["messages"]) != before {
		t.Errorf("снимок увидел сообщение, добавленное после его открытия: %d → %d", before, len(again["messages"]))
	}
	for _, line := range again["contacts"] {
		if strings.Contains(line, "Изменённый") {
			t.Errorf("снимок увидел изменение, сделанное после его открытия: %s", line)
		}
	}
	// Новый снимок видит оба изменения.
	fresh := collect(t, openSnapshot(t, store, a.Tenant))
	if len(fresh["messages"]) != before+1 || !strings.Contains(strings.Join(fresh["contacts"], ""), "Изменённый") {
		t.Errorf("новый снимок не видит зафиксированных изменений")
	}
}

func TestSnapshotOfAnUnknownOrganizationIsEmpty(t *testing.T) {
	store, pools := newStore(t)
	seedOrganization(t, pools.Owner, "a")
	snapshot := openSnapshot(t, store, newID(t))
	if _, err := snapshot.Organization(context.Background()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("Organization() error = %v, want ErrNotFound", err)
	}
	for file, lines := range collect(t, snapshot) {
		if len(lines) != 0 {
			t.Errorf("набор %s чужой организации не пуст: %v", file, lines)
		}
	}
}

func TestSnapshotRowsOfTheTeamHideTheAddressOfAClosedAccount(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	must(t, pools.Owner, `UPDATE users SET email = $2, display_name = 'Удалён', status = 'DISABLED', deleted_at = now() WHERE id = $1`,
		a.Manager, domain.AnonymizedEmail(a.Manager))
	var team []map[string]any
	for _, line := range collect(t, openSnapshot(t, store, a.Tenant))["team"] {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatal(err)
		}
		team = append(team, row)
	}
	if len(team) != 2 {
		t.Fatalf("team = %v", team)
	}
	for _, row := range team {
		switch row["user_id"] {
		case a.Manager:
			if row["email"] != nil || row["display_name"] != "Удалён" {
				t.Errorf("закрытая учётная запись: %v", row)
			}
		case a.Owner:
			if row["email"] != a.OwnerEmail {
				t.Errorf("активный участник потерял адрес: %v", row)
			}
		default:
			t.Errorf("лишний участник: %v", row)
		}
	}
}

func TestSnapshotNeedsAnApplicationPool(t *testing.T) {
	pools := testsupport.PostgresRoles(t)
	store := infrastructure.NewPostgresStore(pools.Platform, nil)
	if _, err := store.OpenSnapshot(context.Background(), newID(t)); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("OpenSnapshot() without the pool error = %v, want ErrInvalid", err)
	}
	if _, err := infrastructure.NewPostgresStore(pools.Platform, pools.App).OpenSnapshot(context.Background(), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("OpenSnapshot() without a tenant error = %v, want ErrInvalid", err)
	}
}

// Условие tenant_id не единственная защита: под ролью приложения RLS не покажет чужие строки и без него.
// Два барьера независимы: условие tenant_id в каждом запросе и RLS. Владелец схемы входит в платформенную роль и RLS
// его не ограничивает, поэтому на его пуле выгрузку организации отделяет от чужой только явное условие.
func TestSnapshotFiltersByTheOrganizationEvenWhenRowLevelSecurityDoesNotApply(t *testing.T) {
	_, pools := newStore(t)
	store := infrastructure.NewPostgresStore(pools.Platform, pools.Owner)
	a := seedOrganization(t, pools.Owner, "a")
	b := seedOrganization(t, pools.Owner, "b")

	var total int
	if err := pools.Owner.QueryRow(context.Background(), `SELECT count(*) FROM messages`).Scan(&total); err != nil || total != 4 {
		t.Fatalf("предпосылка теста: владелец видит %d сообщений, %v; ожидалось 4 (по два в двух организациях)", total, err)
	}
	snapshot := openSnapshot(t, store, a.Tenant)
	organization, err := snapshot.Organization(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var everything strings.Builder
	everything.Write(organization)
	for _, lines := range collect(t, snapshot) {
		everything.WriteString(strings.Join(lines, "\n"))
	}
	output := everything.String()
	if !strings.Contains(output, a.Marker) {
		t.Errorf("содержимое организации a не попало в выгрузку")
	}
	for _, foreign := range []string{b.Marker, b.Tenant, b.Owner, b.Conversation, b.Incoming, b.Contact, b.Name} {
		if strings.Contains(output, foreign) {
			t.Fatalf("условие tenant_id не отделило чужую организацию: в выгрузке %q", foreign)
		}
	}
}

func TestSnapshotTransactionSeesNoForeignRowsEvenWithoutTheTenantCondition(t *testing.T) {
	store, pools := newStore(t)
	a := seedOrganization(t, pools.Owner, "a")
	seedOrganization(t, pools.Owner, "b")
	openSnapshot(t, store, a.Tenant) // снимок открыт и закрывается по завершении теста: незакрытая транзакция держала бы соединение пула
	// Прямой запрос под тем же контекстом и ролью, но без условия, видит только свою организацию.
	tx, err := pools.App.Begin(tenantContext(a.Tenant))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var total, own int
	if err := tx.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE tenant_id = $1::uuid) FROM messages`, a.Tenant).Scan(&total, &own); err != nil {
		t.Fatal(err)
	}
	if total != own || total != 2 {
		t.Errorf("RLS показала %d строк, из них своих %d", total, own)
	}
}
