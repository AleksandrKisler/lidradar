package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"lidradar/backend/internal/admin/application"
	"lidradar/backend/internal/admin/domain"
)

type memoryStore struct {
	admins  map[string]bool
	users   map[string]application.UserRecord
	audits  []domain.AuditEntry
	granted []domain.Admin
	usage   [2]time.Time
	deadJob string
}

func (store *memoryStore) IsAdmin(_ context.Context, userID string) (bool, error) {
	return store.admins[userID], nil
}
func (store *memoryStore) Admins(context.Context) ([]domain.Admin, error) { return store.granted, nil }
func (store *memoryStore) UserByEmail(_ context.Context, email string) (application.UserRecord, bool, error) {
	user, found := store.users[email]
	return user, found, nil
}
func (store *memoryStore) GrantAdmin(_ context.Context, admin domain.Admin, audit domain.AuditEntry) (domain.Admin, bool, error) {
	if store.admins[admin.UserID] {
		return admin, false, nil
	}
	store.admins[admin.UserID] = true
	store.granted = append(store.granted, admin)
	store.audits = append(store.audits, audit)
	return admin, true, nil
}
func (store *memoryStore) RevokeAdmin(_ context.Context, userID string, _ *string, _ time.Time, audit domain.AuditEntry) (bool, error) {
	if !store.admins[userID] {
		return false, nil
	}
	delete(store.admins, userID)
	store.audits = append(store.audits, audit)
	return true, nil
}
func (store *memoryStore) Organizations(context.Context, time.Time) ([]domain.OrganizationSummary, error) {
	return []domain.OrganizationSummary{{ID: "tenant"}}, nil
}
func (store *memoryStore) Connections(context.Context) ([]domain.ConnectionHealth, error) {
	return nil, nil
}
func (store *memoryStore) QueueStats(_ context.Context, now time.Time) (domain.QueueStats, error) {
	return domain.QueueStats{CheckedAt: now, Jobs: domain.LifecycleCounts{Dead: 1}}, nil
}
func (store *memoryStore) Jobs(_ context.Context, filter application.JobFilter) ([]domain.JobRecord, error) {
	return make([]domain.JobRecord, filter.Limit), nil
}
func (store *memoryStore) DeadLetters(context.Context, int) (domain.DeadLetters, error) {
	return domain.DeadLetters{}, nil
}
func (store *memoryStore) RetryJob(_ context.Context, jobID string, at time.Time, audit domain.AuditEntry) (domain.JobRecord, error) {
	if jobID != store.deadJob {
		return domain.JobRecord{}, application.ErrNotFound
	}
	store.audits = append(store.audits, audit)
	return domain.JobRecord{ID: jobID, Status: "PENDING", AvailableAt: at}, nil
}
func (store *memoryStore) DiscardJob(_ context.Context, jobID, _ string, _ time.Time, audit domain.AuditEntry) (domain.JobRecord, error) {
	store.audits = append(store.audits, audit)
	return domain.JobRecord{ID: jobID, Status: "DEAD"}, nil
}
func (store *memoryStore) ReplayEvent(_ context.Context, eventID string, _ time.Time, audit domain.AuditEntry) (domain.OutboxRecord, error) {
	store.audits = append(store.audits, audit)
	return domain.OutboxRecord{ID: eventID, Status: "PENDING"}, nil
}
func (store *memoryStore) DiscardEvent(_ context.Context, eventID, _ string, _ time.Time, audit domain.AuditEntry) (domain.OutboxRecord, error) {
	store.audits = append(store.audits, audit)
	return domain.OutboxRecord{ID: eventID}, nil
}
func (store *memoryStore) RetryAIJob(_ context.Context, jobID string, _ time.Time, audit domain.AuditEntry) (domain.AIJobRecord, error) {
	store.audits = append(store.audits, audit)
	return domain.AIJobRecord{ID: jobID, Status: "PENDING"}, nil
}
func (store *memoryStore) DiscardAIJob(_ context.Context, jobID, _ string, _ time.Time, audit domain.AuditEntry) (domain.AIJobRecord, error) {
	store.audits = append(store.audits, audit)
	return domain.AIJobRecord{ID: jobID}, nil
}
func (store *memoryStore) DiscardDelivery(_ context.Context, deliveryID, _ string, _ time.Time, audit domain.AuditEntry) (domain.DeliveryRecord, error) {
	store.audits = append(store.audits, audit)
	return domain.DeliveryRecord{ID: deliveryID}, nil
}
func (store *memoryStore) AINodes(context.Context) ([]domain.AINode, error) { return nil, nil }
func (store *memoryStore) AIRuns(context.Context, application.RunFilter) ([]domain.AIRun, error) {
	return nil, nil
}
func (store *memoryStore) ConversationSummary(context.Context, string, string) (domain.ConversationSummary, bool, error) {
	return domain.ConversationSummary{}, false, nil
}
func (store *memoryStore) Usage(_ context.Context, from, to time.Time) ([]domain.TenantUsage, error) {
	store.usage = [2]time.Time{from, to}
	return nil, nil
}
func (store *memoryStore) Trace(context.Context, string, string) (domain.Trace, bool, error) {
	return domain.Trace{}, false, nil
}

type sequence struct{ n int }

func (s *sequence) NewID() (string, error) { s.n++; return "id-" + string(rune('a'+s.n)), nil }

func newFixture() (*memoryStore, application.Service) {
	store := &memoryStore{
		admins:  map[string]bool{"root": true},
		users:   map[string]application.UserRecord{"new@example.com": {ID: "new-user", Email: "new@example.com", DisplayName: "Новый"}},
		deadJob: "job-dead",
	}
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	return store, application.NewService(store, new(sequence), func() time.Time { return now })
}

// Любая команда и модель чтения требуют активного PLATFORM_ADMIN; выдача через
// API и CLI пишет аудит с разным источником.
func TestAdminServiceGatesEverythingBehindPlatformAdmin(t *testing.T) {
	store, service := newFixture()
	ctx := context.Background()
	if _, err := service.Organizations(ctx, "manager"); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("не администратор прочитал организации: %v", err)
	}
	if _, err := service.RetryJob(ctx, "manager", "job-dead"); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("не администратор повторил задание: %v", err)
	}
	if admin, err := service.Me(ctx, "manager"); err != nil || admin {
		t.Fatalf("статус не администратора: %v, %v", admin, err)
	}
	granted, created, err := service.Grant(ctx, "root", " New@Example.com ", "поддержка пилота")
	if err != nil || !created || granted.UserID != "new-user" || granted.Email != "new@example.com" || granted.GrantedBy == nil || *granted.GrantedBy != "root" ||
		granted.Note != "поддержка пилота" {
		t.Fatalf("выдача права: %#v, created=%v, %v", granted, created, err)
	}
	if _, created, err := service.Grant(ctx, "root", "new@example.com", ""); err != nil || created {
		t.Fatalf("повторная выдача создала запись: created=%v, %v", created, err)
	}
	if _, _, err := service.Grant(ctx, "root", "ghost@example.com", ""); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("выдача несуществующему пользователю: %v", err)
	}
	if _, err := service.Organizations(ctx, "new-user"); err != nil {
		t.Fatalf("новый администратор не получил доступ: %v", err)
	}
	if revoked, err := service.Revoke(ctx, "root", "new-user"); err != nil || !revoked {
		t.Fatalf("отзыв права: %v, %v", revoked, err)
	}
	if _, err := service.Organizations(ctx, "new-user"); !errors.Is(err, application.ErrForbidden) {
		t.Fatalf("отозванный администратор сохранил доступ: %v", err)
	}
	cli, created, err := service.GrantFromCLI(ctx, "new@example.com", "")
	if err != nil || !created || cli.GrantedBy != nil {
		t.Fatalf("выдача из CLI: %#v, created=%v, %v", cli, created, err)
	}
	if len(store.audits) != 3 || store.audits[0].Source != domain.SourceAPI || store.audits[0].Operation != "PLATFORM_ADMIN_GRANTED" ||
		store.audits[1].Operation != "PLATFORM_ADMIN_REVOKED" || store.audits[2].Source != domain.SourceCLI || store.audits[2].ActorUserID != nil {
		t.Fatalf("аудит выдачи: %#v", store.audits)
	}
}

func TestAdminCommandsAuditAndUsageWindow(t *testing.T) {
	store, service := newFixture()
	ctx := context.Background()
	if job, err := service.RetryJob(ctx, "root", "job-dead"); err != nil || job.Status != "PENDING" {
		t.Fatalf("повтор задания: %#v, %v", job, err)
	}
	if _, err := service.RetryJob(ctx, "root", "job-missing"); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("повтор отсутствующего задания: %v", err)
	}
	if _, err := service.DiscardJob(ctx, "root", "job-dead"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReplayEvent(ctx, "root", "event-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RetryAIJob(ctx, "root", "ai-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.DiscardDelivery(ctx, "root", "delivery-1"); err != nil {
		t.Fatal(err)
	}
	operations := []string{"ADMIN_JOB_RETRIED", "ADMIN_JOB_DISCARDED", "ADMIN_EVENT_REPLAYED", "ADMIN_AI_JOB_RETRIED", "ADMIN_DELIVERY_DISCARDED"}
	if len(store.audits) != len(operations) {
		t.Fatalf("аудит команд: %#v", store.audits)
	}
	for index, operation := range operations {
		if store.audits[index].Operation != operation || store.audits[index].Source != domain.SourceAPI || *store.audits[index].ActorUserID != "root" {
			t.Fatalf("аудит %d = %#v, ожидалась %s", index, store.audits[index], operation)
		}
	}
	report, err := service.Usage(ctx, "root", time.Time{}, time.Time{})
	if err != nil || !report.To.Equal(time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)) || !report.From.Equal(time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)) ||
		report.Tenants == nil || !store.usage[0].Equal(report.From) {
		t.Fatalf("окно потребления по умолчанию: %#v, %v", report, err)
	}
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if _, err := service.Usage(ctx, "root", from, from); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("пустое окно принято: %v", err)
	}
	if _, err := service.Usage(ctx, "root", from, from.AddDate(1, 0, 2)); !errors.Is(err, application.ErrInvalid) {
		t.Fatalf("окно длиннее года принято: %v", err)
	}
	if jobs, err := service.Jobs(ctx, "root", application.JobFilter{Limit: 1000}); err != nil || len(jobs) != application.MaxLimit {
		t.Fatalf("ограничение выборки заданий: %d, %v", len(jobs), err)
	}
	if _, err := service.Summary(ctx, "root", "tenant", "conversation"); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("отсутствующее резюме: %v", err)
	}
}
