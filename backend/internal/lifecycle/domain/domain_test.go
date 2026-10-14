package domain

import (
	"encoding/json"
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Чистые проверки реестра и команд (ADR 0059). Сверку реестра с живой схемой делают тесты хранилища.

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func TestOrganizationDatasetsAreWellFormed(t *testing.T) {
	files, tables := map[string]bool{}, map[string]bool{}
	for _, dataset := range OrganizationDatasets {
		if !identifier.MatchString(strings.ReplaceAll(dataset.File, "-", "_")) || files[dataset.File] {
			t.Errorf("имя файла %q неверно или повторяется", dataset.File)
		}
		files[dataset.File] = true
		if !identifier.MatchString(dataset.Table) || tables[dataset.Table] {
			t.Errorf("таблица %q неверна или повторяется", dataset.Table)
		}
		tables[dataset.Table] = true
		if strings.TrimSpace(dataset.Description) == "" {
			t.Errorf("%s: нет описания для README", dataset.File)
		}
		if len(dataset.Columns) == 0 {
			t.Errorf("%s: нет колонок", dataset.File)
		}
		seen := map[string]bool{}
		for _, column := range dataset.Columns {
			if !identifier.MatchString(column) || seen[column] || column == TenantColumn() {
				t.Errorf("%s: колонка %q неверна, повторяется или это tenant_id", dataset.File, column)
			}
			seen[column] = true
		}
		for _, part := range strings.Split(dataset.Order(), ",") {
			if !identifier.MatchString(strings.TrimSpace(part)) {
				t.Errorf("%s: порядок %q", dataset.File, dataset.Order())
			}
		}
		if _, excluded := ExcludedTables[dataset.Table]; excluded {
			t.Errorf("%s: таблица %s одновременно исключена", dataset.File, dataset.Table)
		}
	}
	if (Dataset{}).Order() != "id" || (Dataset{OrderBy: "created_at, id"}).Order() != "created_at, id" {
		t.Error("порядок строк по умолчанию — id")
	}
	if len(OrganizationDatasets) != 23 {
		t.Errorf("наборов %d: число менять осознанно вместе с ADR 0059 и документацией", len(OrganizationDatasets))
	}
	for _, column := range OrganizationColumns {
		if !identifier.MatchString(column) || column == "id" || column == TenantColumn() {
			t.Errorf("organization.json: колонка %q", column)
		}
	}
}

func TestDatasetByFile(t *testing.T) {
	if dataset, ok := DatasetByFile("messages"); !ok || dataset.Table != "messages" {
		t.Errorf("DatasetByFile(messages) = %+v, %v", dataset, ok)
	}
	if _, ok := DatasetByFile("raw-events"); ok {
		t.Error("служебная таблица не должна быть набором")
	}
}

func TestOnlyTheTeamDatasetHasAJoinedSource(t *testing.T) {
	for _, dataset := range OrganizationDatasets {
		if (dataset.Source == SourceTeam) != (dataset.File == "team") {
			t.Errorf("%s: источник %q", dataset.File, dataset.Source)
		}
	}
}

func TestExcludedTablesAndColumnsExplainThemselves(t *testing.T) {
	for table, reason := range ExcludedTables {
		if strings.TrimSpace(reason) == "" {
			t.Errorf("таблица %s исключена без причины", table)
		}
	}
	for table, columns := range ExcludedColumns {
		for column, reason := range columns {
			if strings.TrimSpace(reason) == "" || column == TenantColumn() {
				t.Errorf("%s.%s: причина %q", table, column, reason)
			}
		}
	}
	// Секреты не могут оказаться среди выгружаемых колонок ни у одного набора.
	for _, dataset := range OrganizationDatasets {
		for _, column := range dataset.Columns {
			for _, secret := range []string{"hash", "secret", "credentials", "password", "token", "object_key"} {
				if strings.Contains(column, secret) {
					t.Errorf("%s: колонка %s похожа на секрет", dataset.File, column)
				}
			}
		}
	}
}

func TestUserReferencesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, reference := range UserReferences {
		key := reference.Table + "." + reference.Column
		if seen[key] || !identifier.MatchString(reference.Table) || !identifier.MatchString(reference.Column) {
			t.Errorf("%s повторяется или названа неверно", key)
		}
		seen[key] = true
		switch reference.Disposition {
		case Erase, Anonymize, Revoke, Retain:
		default:
			t.Errorf("%s: действие %q", key, reference.Disposition)
		}
		if (reference.Disposition == Retain || reference.Disposition == Revoke) && reference.Export == "" && strings.TrimSpace(reference.Reason) == "" &&
			!strings.HasPrefix(reference.Table, "membership_invitations") && !strings.HasPrefix(reference.Table, "platform_") {
			t.Errorf("%s остаётся и не выгружается, но причина не названа", key)
		}
	}
	for _, mandatory := range []string{"users.id", "sessions.user_id", "memberships.user_id", "auth_audit_log.user_id", "telegram_user_links.user_id"} {
		if !seen[mandatory] {
			t.Errorf("в реестре нет %s", mandatory)
		}
	}
}

func TestAnonymizedEmailIsUndeliverableCanonicalAndUnique(t *testing.T) {
	first, second := "0199fa32-5c41-7a10-8d5e-1b2c3d4e5f60", "0199fa32-5c41-7a10-8d5e-1b2c3d4e5f61"
	address := AnonymizedEmail(first)
	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		t.Fatalf("адрес %q не разбирается как простой адрес: %v", address, err)
	}
	if !strings.HasSuffix(address, ".invalid") || address != strings.ToLower(strings.TrimSpace(address)) || len(address) > 254 {
		t.Errorf("адрес %q должен быть в зоне .invalid, в нижнем регистре и не длиннее 254", address)
	}
	if AnonymizedEmail(second) == address {
		t.Error("адреса двух пользователей совпали")
	}
	if AnonymizedDisplayName != "Удалён" {
		t.Errorf("имя обезличенного пользователя = %q", AnonymizedDisplayName)
	}
}

func TestErasureValidate(t *testing.T) {
	at := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	due := Erasure{UserID: "u", Mode: ModeDue, At: at, PasswordHash: "h", AuditID: "a"}
	operator := Erasure{UserID: "u", Mode: ModeImmediate, At: at, PasswordHash: "h", AuditID: "a", AdminAuditID: "b", Note: "обращение"}
	if due.Validate() != nil || operator.Validate() != nil {
		t.Fatal("корректные команды отклонены")
	}
	dry := operator
	dry.DryRun = true
	if dry.Validate() != nil {
		t.Error("сухой запуск оператора отклонён")
	}
	for name, command := range map[string]Erasure{
		"без пользователя": func() Erasure { c := due; c.UserID = ""; return c }(),
		"без времени":      func() Erasure { c := due; c.At = time.Time{}; return c }(),
		"без хэша":         func() Erasure { c := due; c.PasswordHash = ""; return c }(),
		"без аудита":       func() Erasure { c := due; c.AuditID = ""; return c }(),
		"режим":            func() Erasure { c := due; c.Mode = ""; return c }(),
		"сухой по сроку":   func() Erasure { c := due; c.DryRun = true; return c }(),
		"оператор без записи оператора": func() Erasure { c := operator; c.AdminAuditID = ""; return c }(),
		"оператор без заметки":          func() Erasure { c := operator; c.Note = ""; return c }(),
	} {
		if err := command.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
}

func TestSectionEncodesAnEmptyListAsAnArray(t *testing.T) {
	type item struct {
		Name string `json:"name"`
	}
	for name, section := range map[string]Section[item]{
		"nil":    {},
		"пустой": {Items: []item{}},
	} {
		encoded, err := json.Marshal(section)
		if err != nil || string(encoded) != `{"items":[],"truncated":false}` {
			t.Errorf("%s: %s, %v", name, encoded, err)
		}
	}
	encoded, _ := json.Marshal(Section[item]{Items: []item{{Name: "a"}}, Truncated: true})
	if string(encoded) != `{"items":[{"name":"a"}],"truncated":true}` {
		t.Errorf("section = %s", encoded)
	}
}

func TestPersonalExportNeverNamesAnOrganizationIdentifier(t *testing.T) {
	encoded, err := json.Marshal(PersonalExport{})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"tenant", "organizationId", "passwordHash", "tokenHash"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("документ личной выгрузки содержит поле %q", forbidden)
		}
	}
}
