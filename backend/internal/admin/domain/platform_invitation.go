package domain

import (
	"strings"
	"time"
	"unicode/utf8"
)

// Параметры кода платформы (ADR 0057).
const (
	PlatformInvitationDefaultDays = 14
	PlatformInvitationMaxDays     = 60
	PlatformInvitationNoteLimit   = 500
	// MaxOrganizationRights ограничивает право создать организации у одного пользователя (CHECK в схеме).
	MaxOrganizationRights = 100
)

type PlatformInvitationStatus string

const (
	PlatformInvitationPending PlatformInvitationStatus = "PENDING"
	PlatformInvitationUsed    PlatformInvitationStatus = "USED"
	PlatformInvitationExpired PlatformInvitationStatus = "EXPIRED"
	PlatformInvitationRevoked PlatformInvitationStatus = "REVOKED"
)

// PlatformInvitation — одноразовый код платформы: даёт право зарегистрироваться и создать одну
// организацию. Сам код показывается оператору один раз и хранится только как SHA-256; строка не
// удаляется, погашение и отзыв фиксируются отметками. Погашает код модуль identity при регистрации.
type PlatformInvitation struct {
	ID         string     `json:"id"`
	CodeHash   string     `json:"-"`
	Note       *string    `json:"note"`
	CreatedAt  time.Time  `json:"createdAt"`
	ExpiresAt  time.Time  `json:"expiresAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	ConsumedAt *time.Time `json:"consumedAt"`
	ConsumedBy *string    `json:"consumedBy"`
}

// NewPlatformInvitation собирает код со сроком в сутках; пустая заметка не сохраняется.
func NewPlatformInvitation(id, codeHash, note string, days int, at time.Time) (PlatformInvitation, error) {
	if days <= 0 {
		days = PlatformInvitationDefaultDays
	}
	var cleaned *string
	if trimmed := strings.TrimSpace(note); trimmed != "" {
		cleaned = &trimmed
	}
	invitation := PlatformInvitation{
		ID: id, CodeHash: codeHash, Note: cleaned, CreatedAt: at.UTC(),
		ExpiresAt: at.UTC().Add(time.Duration(days) * 24 * time.Hour),
	}
	if days > PlatformInvitationMaxDays || invitation.Validate() != nil {
		return PlatformInvitation{}, ErrInvalid
	}
	return invitation, nil
}

func (invitation PlatformInvitation) Validate() error {
	if invitation.ID == "" || len(invitation.CodeHash) != 64 || invitation.CreatedAt.IsZero() ||
		!invitation.ExpiresAt.After(invitation.CreatedAt) {
		return ErrInvalid
	}
	if invitation.Note != nil && (*invitation.Note == "" || *invitation.Note != strings.TrimSpace(*invitation.Note) ||
		utf8.RuneCountInString(*invitation.Note) > PlatformInvitationNoteLimit) {
		return ErrInvalid
	}
	if (invitation.ConsumedAt == nil) != (invitation.ConsumedBy == nil) ||
		(invitation.ConsumedAt != nil && invitation.RevokedAt != nil) {
		return ErrInvalid
	}
	return nil
}

// Status выводит состояние на момент времени: отметки погашения и отзыва важнее срока.
func (invitation PlatformInvitation) Status(now time.Time) PlatformInvitationStatus {
	switch {
	case invitation.ConsumedAt != nil:
		return PlatformInvitationUsed
	case invitation.RevokedAt != nil:
		return PlatformInvitationRevoked
	case !now.Before(invitation.ExpiresAt):
		return PlatformInvitationExpired
	default:
		return PlatformInvitationPending
	}
}
