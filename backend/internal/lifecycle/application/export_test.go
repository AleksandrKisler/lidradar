package application

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"lidradar/backend/internal/lifecycle/domain"
)

type fixture struct {
	journal     *journal
	store       *fakeStore
	snapshot    *fakeSnapshot
	permissions *fakePermissions
	auditor     *fakeAuditor
	limiter     *fakeLimiter
	mailer      *fakeMailer
	passwords   *fakePasswords
	reporter    *fakeReporter
	ids         *fakeIDs
	now         time.Time
	service     Service
}

func newFixture() *fixture {
	journal := &journal{}
	f := &fixture{
		journal: journal,
		now:     time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC),
		ids:     &fakeIDs{},
		mailer:  &fakeMailer{}, passwords: &fakePasswords{}, reporter: &fakeReporter{},
	}
	f.snapshot = &fakeSnapshot{
		journal: journal, at: f.now.Add(-time.Minute), org: []byte(`{"name":"Салон"}`),
		rows: map[string][]string{}, rowsErr: map[string]error{},
	}
	for _, dataset := range domain.OrganizationDatasets {
		f.snapshot.rows[dataset.File] = []string{
			fmt.Sprintf(`{"id":"%s-1","text":"первая"}`, dataset.File),
			fmt.Sprintf(`{"id":"%s-2","text":"вторая"}`, dataset.File),
		}
	}
	f.store = &fakeStore{
		journal: journal, snapshot: f.snapshot,
		results: map[string]domain.ErasureResult{}, failures: map[string]error{}, users: map[string]string{},
	}
	f.permissions = &fakePermissions{journal: journal, allowed: true}
	f.auditor = &fakeAuditor{journal: journal}
	f.limiter = &fakeLimiter{journal: journal, allowed: true}
	f.service = NewService(f.store, f.passwords, f.ids, func() time.Time { return f.now }).
		WithOrganizationExport(f.permissions, f.auditor, f.limiter).
		WithMail(f.mailer, "https://app.example.test/").
		WithReporter(f.reporter)
	return f
}

// firstByteWriter записывает в журнал момент первой записи.
type firstByteWriter struct {
	buffer  bytes.Buffer
	journal *journal
	noted   bool
	failAt  int // если > 0, запись после стольких байт возвращает ошибку «клиент ушёл»
}

var errClientGone = errors.New("client went away")

func (w *firstByteWriter) Write(data []byte) (int, error) {
	if !w.noted {
		w.noted = true
		w.journal.add("first-byte")
	}
	if w.failAt > 0 && w.buffer.Len()+len(data) > w.failAt {
		return 0, errClientGone
	}
	return w.buffer.Write(data)
}

func openArchive(t *testing.T, body []byte) *zip.Reader {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("архив не открывается: %v", err)
	}
	return archive
}

func readEntry(t *testing.T, file *zip.File) []byte {
	t.Helper()
	reader, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func TestExportOrganizationWritesAWholeArchiveWithAnHonestManifest(t *testing.T) {
	f := newFixture()
	out := &firstByteWriter{journal: f.journal}
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); err != nil {
		t.Fatalf("ExportOrganization() error = %v", err)
	}
	archive := openArchive(t, out.buffer.Bytes())

	var names []string
	for _, file := range archive.File {
		names = append(names, file.Name)
		if file.Method != zip.Deflate {
			t.Errorf("%s: метод %d, ожидалось сжатие Deflate", file.Name, file.Method)
		}
		if !file.Modified.Equal(f.snapshot.at) {
			t.Errorf("%s: время файла %s, ожидался момент снимка %s", file.Name, file.Modified, f.snapshot.at)
		}
	}
	want := []string{"README.txt", "organization.json"}
	for _, dataset := range domain.OrganizationDatasets {
		want = append(want, "data/"+dataset.File+".jsonl")
	}
	want = append(want, "manifest.json")
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Fatalf("состав архива:\n%v\nожидался:\n%v", names, want)
	}

	// Манифест лежит последним и описывает все остальные файлы, кроме себя.
	var manifest domain.Manifest
	if err := json.Unmarshal(readEntry(t, archive.File[len(archive.File)-1]), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Format != "lidradar.organization-export/1" || !manifest.GeneratedAt.Equal(f.snapshot.at) || len(manifest.Files) != len(archive.File)-1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	for index, entry := range manifest.Files {
		file := archive.File[index]
		if entry.Name != file.Name {
			t.Errorf("манифест %d: %s, в архиве %s", index, entry.Name, file.Name)
		}
		content := readEntry(t, file)
		sum := sha256.Sum256(content)
		if entry.Bytes != int64(len(content)) || entry.SHA256 != hex.EncodeToString(sum[:]) {
			t.Errorf("%s: манифест %d байт %s, фактически %d байт %x", file.Name, entry.Bytes, entry.SHA256, len(content), sum)
		}
		wantRows := int64(2)
		switch file.Name {
		case "README.txt":
			wantRows = 0
		case "organization.json":
			wantRows = 1
		}
		if entry.Rows != wantRows {
			t.Errorf("%s: строк в манифесте %d, ожидалось %d", file.Name, entry.Rows, wantRows)
		}
		if strings.HasSuffix(file.Name, ".jsonl") {
			lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
			if int64(len(lines)) != entry.Rows {
				t.Errorf("%s: строк в файле %d, в манифесте %d", file.Name, len(lines), entry.Rows)
			}
			for _, line := range lines {
				var object map[string]any
				if err := json.Unmarshal([]byte(line), &object); err != nil {
					t.Errorf("%s: строка не JSON: %q", file.Name, line)
				}
			}
		}
	}
	if document := string(readEntry(t, archive.File[1])); !strings.Contains(document, `"name":"Салон"`) || !strings.HasSuffix(document, "\n") {
		t.Errorf("organization.json = %q", document)
	}
}

func TestExportOrganizationReadmeNamesEveryFileAndWhatIsLeftOut(t *testing.T) {
	f := newFixture()
	out := &firstByteWriter{journal: f.journal}
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); err != nil {
		t.Fatal(err)
	}
	readme := string(readEntry(t, openArchive(t, out.buffer.Bytes()).File[0]))
	for _, dataset := range domain.OrganizationDatasets {
		if !strings.Contains(readme, "data/"+dataset.File+".jsonl — "+dataset.Description) {
			t.Errorf("README не описывает %s", dataset.File)
		}
	}
	for _, want := range []string{
		"lidradar.organization-export/1", "2026-10-14T08:59:00Z", "manifest.json", "оборван", "секреты и реквизиты доступа",
		"служебные очереди", "личной выгрузке", "UTC", "не повторяется",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README не содержит %q", want)
		}
	}
}

func TestExportOrganizationChecksBeforeAnyByteAndAuditsTheAttempt(t *testing.T) {
	f := newFixture()
	out := &firstByteWriter{journal: f.journal}
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); err != nil {
		t.Fatal(err)
	}
	// Право, предел, аудит и снимок идут раньше первого байта; снимок закрывается в конце.
	if got, want := f.journal.text(), "permission,limit,audit,open,first-byte,close"; got != want {
		t.Fatalf("порядок: %s, ожидался %s", got, want)
	}
	if len(f.auditor.entries) != 1 {
		t.Fatalf("записей аудита: %d", len(f.auditor.entries))
	}
	entry := f.auditor.entries[0]
	if entry.TenantID != "tenant-1" || entry.ActorID != "actor-1" || entry.Operation != "ORGANIZATION_DATA_EXPORTED" ||
		entry.EntityType != "ORGANIZATION" || entry.EntityID != "tenant-1" || !entry.At.Equal(f.now) {
		t.Errorf("запись аудита = %+v", entry)
	}
	if f.permissions.asked[0] != "actor-1|tenant-1|organization.export" {
		t.Errorf("проверено право %q", f.permissions.asked)
	}
	if f.limiter.taken[0] != "ORGANIZATION_EXPORT_TENANT" || f.limiter.limit != 3 || f.limiter.window != time.Hour {
		t.Errorf("предел: %v %d за %s", f.limiter.taken, f.limiter.limit, f.limiter.window)
	}
	// Предел считается по организации, но идентификатор в таблицу не пишется открытым.
	if subject := f.limiter.subjects[0]; strings.Contains(subject, "tenant-1") || len(subject) != 64 {
		t.Errorf("субъект предела = %q, ожидался SHA-256 организации", subject)
	}
	if f.store.openFor[0] != "tenant-1" {
		t.Errorf("снимок открыт для %v", f.store.openFor)
	}
}

func TestExportOrganizationRefusesWithoutThePermission(t *testing.T) {
	f := newFixture()
	f.permissions.allowed = false
	out := &firstByteWriter{journal: f.journal}
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); !errors.Is(err, ErrForbidden) {
		t.Fatalf("error = %v, want ErrForbidden", err)
	}
	if got := f.journal.text(); got != "permission" {
		t.Errorf("после отказа в праве выполнено лишнее: %s", got)
	}
	if out.buffer.Len() != 0 {
		t.Error("отказ записал байты")
	}
	f.permissions.err = errors.New("permissions unavailable")
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); err == nil || errors.Is(err, ErrForbidden) {
		t.Errorf("сбой проверки права должен остаться сбоем, а не отказом: %v", err)
	}
}

func TestExportOrganizationRateLimitReportsWhenToRetry(t *testing.T) {
	f := newFixture()
	f.limiter.allowed, f.limiter.expires = false, 40*time.Minute
	out := &firstByteWriter{journal: f.journal}
	err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("error = %v, want ErrRateLimited", err)
	}
	if retry := RateLimitRetryAfter(err); retry != 40*time.Minute {
		t.Errorf("retry after = %s", retry)
	}
	if got := f.journal.text(); got != "permission,limit" {
		t.Errorf("исчерпанный предел не остановил выгрузку: %s", got)
	}
	if RateLimitRetryAfter(errors.New("other")) != time.Second {
		t.Error("без срока повтор не меньше секунды")
	}
	f.limiter.err = errors.New("limiter unavailable")
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); err == nil || errors.Is(err, ErrRateLimited) {
		t.Errorf("сбой предела должен остаться сбоем: %v", err)
	}
}

func TestExportOrganizationWithoutATraceProducesNothing(t *testing.T) {
	f := newFixture()
	f.auditor.err = errors.New("audit unavailable")
	out := &firstByteWriter{journal: f.journal}
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); err == nil {
		t.Fatal("выгрузка выполнена без записи аудита")
	}
	if got := f.journal.text(); got != "permission,limit,audit" || out.buffer.Len() != 0 {
		t.Errorf("после сбоя аудита: %s, записано %d байт", got, out.buffer.Len())
	}
}

func TestExportOrganizationSnapshotFailureHappensBeforeTheFirstByte(t *testing.T) {
	f := newFixture()
	f.store.openErr = errors.New("database unavailable")
	out := &firstByteWriter{journal: f.journal}
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); err == nil {
		t.Fatal("выгрузка выполнена без снимка")
	}
	if out.noted {
		t.Error("снимок не открылся, но байты уже пошли: транспорт не сможет ответить ошибкой")
	}
}

func TestExportOrganizationBrokenMidwayReturnsTheErrorAndLeavesNoManifest(t *testing.T) {
	f := newFixture()
	boom := errors.New("connection lost")
	f.snapshot.rowsErr["messages"] = boom
	out := &firstByteWriter{journal: f.journal}
	err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out)
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the database error", err)
	}
	if _, openErr := zip.NewReader(bytes.NewReader(out.buffer.Bytes()), int64(out.buffer.Len())); openErr == nil {
		t.Error("оборванный архив открывается как целый")
	}
	if bytes.Contains(out.buffer.Bytes(), []byte("manifest.json")) {
		t.Error("у оборванного архива есть манифест")
	}
	if !f.snapshot.closed {
		t.Error("снимок не закрыт после сбоя")
	}
	for _, later := range []string{"attachments", "audit-log"} {
		for _, called := range f.snapshot.rowsCalls {
			if called == later {
				t.Errorf("чтение продолжилось после сбоя: набор %s", later)
			}
		}
	}
}

func TestExportOrganizationStopsWhenTheClientGoesAway(t *testing.T) {
	f := newFixture()
	out := &firstByteWriter{journal: f.journal, failAt: 600}
	err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out)
	if !errors.Is(err, errClientGone) {
		t.Fatalf("error = %v, want the write error", err)
	}
	if len(f.snapshot.rowsCalls) == len(domain.OrganizationDatasets) {
		t.Error("отключённый клиент не остановил чтение наборов")
	}
	if !f.snapshot.closed {
		t.Error("снимок не закрыт после ухода клиента")
	}
}

func TestExportOrganizationStopsWhenTheContextIsCancelled(t *testing.T) {
	f := newFixture()
	ctx, cancel := context.WithCancel(context.Background())
	f.snapshot.onRows = func(dataset domain.Dataset) error {
		if dataset.File == "contacts" {
			cancel()
		}
		return nil
	}
	out := &firstByteWriter{journal: f.journal}
	err := f.service.ExportOrganization(ctx, "actor-1", "tenant-1", out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if bytes.Contains(out.buffer.Bytes(), []byte("manifest.json")) {
		t.Error("отменённая выгрузка дописала манифест")
	}
	if !f.snapshot.closed {
		t.Error("снимок не закрыт после отмены")
	}
}

func TestExportOrganizationBoundsTheWholeRunByATimeout(t *testing.T) {
	f := newFixture()
	out := &firstByteWriter{journal: f.journal}
	started := time.Now()
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); err != nil {
		t.Fatal(err)
	}
	deadline, ok := f.store.openCtx.Deadline()
	if !ok {
		t.Fatal("у выгрузки нет предела времени")
	}
	if limit := deadline.Sub(started); limit > domain.ExportTimeout+time.Second || limit < domain.ExportTimeout-time.Minute {
		t.Errorf("предел времени %s, ожидалось около %s", limit, domain.ExportTimeout)
	}
}

func TestExportOrganizationLimitsConcurrentRunsAndReleasesTheSlot(t *testing.T) {
	f := newFixture()
	f.service = f.service.WithExportSlots(1)
	entered, release := make(chan struct{}), make(chan struct{})
	f.snapshot.onRows = func(dataset domain.Dataset) error {
		if dataset.File == "team" {
			close(entered)
			<-release
		}
		return nil
	}
	var running sync.WaitGroup
	running.Add(1)
	firstResult := make(chan error, 1)
	go func() {
		defer running.Done()
		firstResult <- f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", &firstByteWriter{journal: &journal{}})
	}()
	<-entered

	// Пока идёт первая выгрузка, вторая получает отказ и не тратит предел организации.
	f2 := f.limiter.taken
	err := f.service.ExportOrganization(context.Background(), "actor-2", "tenant-2", &firstByteWriter{journal: &journal{}})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("параллельная выгрузка error = %v, want ErrBusy", err)
	}
	if len(f.limiter.taken) != len(f2) {
		t.Error("отказ по занятости потратил предел")
	}
	close(release)
	running.Wait()
	if err := <-firstResult; err != nil {
		t.Fatalf("первая выгрузка error = %v", err)
	}
	// Слот освобождён: следующая выгрузка идёт.
	f.snapshot.onRows = nil
	if err := f.service.ExportOrganization(context.Background(), "actor-3", "tenant-3", &firstByteWriter{journal: &journal{}}); err != nil {
		t.Fatalf("выгрузка после освобождения слота error = %v", err)
	}
}

func TestExportOrganizationReleasesTheSlotAfterAFailure(t *testing.T) {
	f := newFixture()
	f.service = f.service.WithExportSlots(1)
	f.snapshot.rowsErr["contacts"] = errors.New("boom")
	for attempt := 0; attempt < 3; attempt++ {
		err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", &firstByteWriter{journal: &journal{}})
		if err == nil || errors.Is(err, ErrBusy) {
			t.Fatalf("попытка %d: error = %v, слот должен освобождаться после сбоя", attempt, err)
		}
	}
}

func TestExportOrganizationNeedsEveryDependency(t *testing.T) {
	f := newFixture()
	out := &firstByteWriter{journal: f.journal}
	bare := NewService(f.store, f.passwords, f.ids, func() time.Time { return f.now })
	if err := bare.ExportOrganization(context.Background(), "actor-1", "tenant-1", out); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("служба без зависимостей error = %v, want ErrNotConfigured", err)
	}
	if err := f.service.ExportOrganization(context.Background(), "", "tenant-1", out); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("без пользователя error = %v", err)
	}
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "", out); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("без организации error = %v", err)
	}
	if err := f.service.ExportOrganization(context.Background(), "actor-1", "tenant-1", nil); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("без приёмника error = %v", err)
	}
}

func TestPersonalExportAddsTheHeaderAndKeepsEveryListAnArray(t *testing.T) {
	f := newFixture()
	f.store.personal = domain.PersonalExport{
		Account:     domain.ExportAccount{ID: "user-1", Email: "user@example.test", DisplayName: "Пользователь", Status: "ACTIVE", CreatedAt: f.now.Add(-time.Hour)},
		Memberships: domain.Section[domain.ExportMembership]{Items: []domain.ExportMembership{{Organization: "Салон", Role: "OWNER", Status: "ACTIVE", JoinedAt: f.now}}},
	}
	body, err := f.service.PersonalExport(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(body, []byte("}\n")) || !bytes.Contains(body, []byte("\n  \"format\"")) {
		t.Errorf("документ должен быть читаемым JSON с переводом строки в конце: %q", body)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	var format, note string
	var generated time.Time
	_ = json.Unmarshal(document["format"], &format)
	_ = json.Unmarshal(document["note"], &note)
	_ = json.Unmarshal(document["generatedAt"], &generated)
	if format != "lidradar.account-export/1" || !generated.Equal(f.now) || !strings.Contains(note, "принадлежит организации") {
		t.Errorf("шапка: %q %s %q", format, generated, note)
	}
	for _, section := range []string{"memberships", "sessions", "authEvents", "notificationSettings", "telegramLinks", "consentDecisions"} {
		var parsed struct {
			Items     *[]json.RawMessage `json:"items"`
			Truncated bool               `json:"truncated"`
		}
		if err := json.Unmarshal(document[section], &parsed); err != nil || parsed.Items == nil {
			t.Errorf("%s: items должен быть массивом, а не null: %s", section, document[section])
		}
	}
	if string(document["authoredRecords"]) == "null" {
		t.Error("authoredRecords закодирован как null")
	}
	if strings.Contains(string(body), "tenant") {
		t.Errorf("в личной выгрузке есть поле организации: %s", body)
	}
	if f.store.personalFor[0] != "user-1" {
		t.Errorf("выгрузка запрошена для %v", f.store.personalFor)
	}
}

func TestPersonalExportPassesStoreErrorsAndRejectsEmptyInput(t *testing.T) {
	f := newFixture()
	f.store.personalErr = domain.ErrNotFound
	if _, err := f.service.PersonalExport(context.Background(), "user-1"); !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
	if _, err := f.service.PersonalExport(context.Background(), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("error = %v, want ErrInvalid", err)
	}
	if _, err := (Service{}).PersonalExport(context.Background(), "user-1"); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("error = %v, want ErrNotConfigured", err)
	}
}

func TestDeletionBlockersMapToTheIdentityContract(t *testing.T) {
	f := newFixture()
	f.store.blockers = []domain.Blocker{
		{Code: "SOLE_OWNER", OrganizationName: "Салон", HasOtherMembers: true},
		{Code: "PLATFORM_ADMIN"},
	}
	got, err := f.service.DeletionBlockers(context.Background(), "user-1")
	if err != nil || len(got) != 2 {
		t.Fatalf("DeletionBlockers() = %+v, %v", got, err)
	}
	if got[0].Code != "SOLE_OWNER" || got[0].OrganizationName != "Салон" || !got[0].HasOtherMembers || got[1].Code != "PLATFORM_ADMIN" {
		t.Errorf("blockers = %+v", got)
	}
	f.store.blockers = nil
	if got, err := f.service.DeletionBlockers(context.Background(), "user-1"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("без блокировок ожидался пустой непустой-nil список: %#v, %v", got, err)
	}
	f.store.blockersErr = errors.New("database unavailable")
	if _, err := f.service.DeletionBlockers(context.Background(), "user-1"); err == nil {
		t.Error("сбой хранилища потерян")
	}
	if _, err := f.service.DeletionBlockers(context.Background(), ""); !errors.Is(err, domain.ErrInvalid) {
		t.Errorf("error = %v", err)
	}
}
