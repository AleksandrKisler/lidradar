package application_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"lidradar/backend/internal/admin/application"
	"lidradar/backend/internal/admin/domain"
)

var platformCodePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// Код платформы (ADR 0057): открытый код возвращается один раз, в хранилище только его SHA-256,
// в аудите нет ни кода, ни заметки.
func TestIssuingAPlatformInvitationStoresOnlyTheHashAndAuditsWithoutSecrets(t *testing.T) {
	store, service := newFixture()
	invitation, code, err := service.IssuePlatformInvitationFromCLI(context.Background(), "  Салон «Линия»  ", 0)
	if err != nil {
		t.Fatalf("IssuePlatformInvitationFromCLI() = %v", err)
	}
	if !platformCodePattern.MatchString(code) {
		t.Fatalf("код %q не в формате кодов приглашений", code)
	}
	digest := sha256.Sum256([]byte(code))
	stored := store.invitations[invitation.ID]
	if stored.CodeHash != hex.EncodeToString(digest[:]) || strings.Contains(stored.CodeHash, code) {
		t.Fatalf("в хранилище %q: ожидался SHA-256 кода", stored.CodeHash)
	}
	if stored.Note == nil || *stored.Note != "Салон «Линия»" {
		t.Fatalf("заметка = %v, ожидалась очищенная строка", stored.Note)
	}
	if want := invitation.CreatedAt.Add(domain.PlatformInvitationDefaultDays * 24 * time.Hour); !stored.ExpiresAt.Equal(want) {
		t.Fatalf("срок = %s, want %s (по умолчанию %d суток)", stored.ExpiresAt, want, domain.PlatformInvitationDefaultDays)
	}
	if len(store.audits) != 1 {
		t.Fatalf("записей аудита %d, ожидалась одна", len(store.audits))
	}
	audit := store.audits[0]
	if audit.Operation != "PLATFORM_INVITATION_ISSUED" || audit.Source != domain.SourceCLI || audit.ActorUserID != nil ||
		audit.EntityType != "PLATFORM_INVITATION" || audit.EntityID != invitation.ID {
		t.Fatalf("аудит = %+v", audit)
	}
	if rendered := audit.Operation + audit.EntityType + audit.EntityID; strings.Contains(rendered, code) || len(audit.Details) != 0 {
		t.Fatalf("аудит содержит лишнее: %+v", audit)
	}
}

func TestEachPlatformInvitationGetsADifferentCode(t *testing.T) {
	_, service := newFixture()
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		_, code, err := service.IssuePlatformInvitationFromCLI(context.Background(), "", 1)
		if err != nil || seen[code] {
			t.Fatalf("выпуск %d: код повторился или ошибка %v", i, err)
		}
		seen[code] = true
	}
}

func TestPlatformInvitationTermIsBounded(t *testing.T) {
	_, service := newFixture()
	for name, days := range map[string]int{"слишком долго": domain.PlatformInvitationMaxDays + 1} {
		if _, _, err := service.IssuePlatformInvitationFromCLI(context.Background(), "", days); !errors.Is(err, application.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if _, _, err := service.IssuePlatformInvitationFromCLI(context.Background(), strings.Repeat("я", domain.PlatformInvitationNoteLimit+1), 5); !errors.Is(err, application.ErrInvalid) {
		t.Errorf("слишком длинная заметка: %v, want ErrInvalid", err)
	}
	if _, _, err := service.IssuePlatformInvitationFromCLI(context.Background(), strings.Repeat("я", domain.PlatformInvitationNoteLimit), domain.PlatformInvitationMaxDays); err != nil {
		t.Errorf("граничные значения должны проходить: %v", err)
	}
}

func TestRevokingAPlatformInvitation(t *testing.T) {
	store, service := newFixture()
	invitation, _, err := service.IssuePlatformInvitationFromCLI(context.Background(), "", 7)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := service.RevokePlatformInvitationFromCLI(context.Background(), invitation.ID)
	if err != nil || revoked.RevokedAt == nil {
		t.Fatalf("отзыв: %v %+v", err, revoked)
	}
	if again, err := service.RevokePlatformInvitationFromCLI(context.Background(), invitation.ID); err != nil || again.RevokedAt == nil {
		t.Fatalf("повторный отзыв должен быть идемпотентным: %v", err)
	}
	if got := countAudits(store, "PLATFORM_INVITATION_REVOKED"); got != 1 {
		t.Fatalf("записей об отзыве %d, ожидалась одна (повтор не пишет аудит)", got)
	}
	if _, err := service.RevokePlatformInvitationFromCLI(context.Background(), "unknown"); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("неизвестный код: %v, want ErrNotFound", err)
	}
	used, _, err := service.IssuePlatformInvitationFromCLI(context.Background(), "", 7)
	if err != nil {
		t.Fatal(err)
	}
	consumedAt, consumer := time.Now(), "user-1"
	record := store.invitations[used.ID]
	record.ConsumedAt, record.ConsumedBy = &consumedAt, &consumer
	store.invitations[used.ID] = record
	if _, err := service.RevokePlatformInvitationFromCLI(context.Background(), used.ID); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("отзыв использованного кода: %v, want ErrConflict", err)
	}
}

func TestAllowingAnOrganizationAddsOneRightPerCommand(t *testing.T) {
	store, service := newFixture()
	for want := 1; want <= 2; want++ {
		user, rights, err := service.AllowOrganizationFromCLI(context.Background(), " NEW@example.com ")
		if err != nil || rights != want || user.ID != "new-user" {
			t.Fatalf("выдача %d: %v, прав %d, пользователь %+v", want, err, rights, user)
		}
	}
	if got := countAudits(store, "ORGANIZATION_RIGHT_GRANTED"); got != 2 {
		t.Fatalf("записей аудита %d, ожидалось две", got)
	}
	if _, _, err := service.AllowOrganizationFromCLI(context.Background(), "nobody@example.com"); !errors.Is(err, application.ErrNotFound) {
		t.Fatalf("неизвестная почта: %v, want ErrNotFound", err)
	}
	store.rights["new-user"] = domain.MaxOrganizationRights
	if _, _, err := service.AllowOrganizationFromCLI(context.Background(), "new@example.com"); !errors.Is(err, application.ErrConflict) {
		t.Fatalf("предел прав: %v, want ErrConflict", err)
	}
}

func TestPlatformInvitationStatusFollowsMarksBeforeTheTerm(t *testing.T) {
	at := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	invitation, err := domain.NewPlatformInvitation("id-1", strings.Repeat("a", 64), "", 1, at)
	if err != nil {
		t.Fatal(err)
	}
	if invitation.Status(at) != domain.PlatformInvitationPending || invitation.Status(at.Add(24*time.Hour)) != domain.PlatformInvitationExpired {
		t.Fatal("ожидание и срок определены неверно")
	}
	revoked := at.Add(time.Hour)
	invitation.RevokedAt = &revoked
	if invitation.Status(at.Add(48*time.Hour)) != domain.PlatformInvitationRevoked {
		t.Fatal("отзыв важнее срока")
	}
	invitation.RevokedAt = nil
	consumed, user := at.Add(time.Hour), "u"
	invitation.ConsumedAt, invitation.ConsumedBy = &consumed, &user
	if invitation.Status(at.Add(48*time.Hour)) != domain.PlatformInvitationUsed {
		t.Fatal("использование важнее срока")
	}
	invitation.RevokedAt = &revoked
	if invitation.Validate() == nil {
		t.Fatal("использованный и отозванный одновременно — недопустимое состояние")
	}
}

func countAudits(store *memoryStore, operation string) int {
	count := 0
	for _, audit := range store.audits {
		if audit.Operation == operation {
			count++
		}
	}
	return count
}
