// Package domain описывает записи аудита критических действий (ТЗ §65):
// действия участника в организации и события входа пользователя.
package domain

import (
	"errors"
	"regexp"
	"strings"
	"time"
)

var ErrInvalid = errors.New("некорректная запись аудита")

var namePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,99}$`)

// Entry — действие участника над сущностью организации; хранится в
// append-only журнале организации рядом с действиями, исходами и выручкой.
type Entry struct {
	ID         string
	TenantID   string
	ActorID    string
	Operation  string
	EntityType string
	EntityID   string
	At         time.Time
}

func (entry Entry) Validate() error {
	if entry.ID == "" || entry.TenantID == "" || entry.ActorID == "" || entry.EntityID == "" || entry.At.IsZero() ||
		!namePattern.MatchString(entry.Operation) || !namePattern.MatchString(entry.EntityType) {
		return ErrInvalid
	}
	return nil
}

// AuthEntry — событие входа, выхода или регистрации пользователя вне
// организации. Секреты и заголовки не сохраняются (§64), только адрес.
type AuthEntry struct {
	ID        string
	UserID    string
	Operation string
	IPAddress string
	At        time.Time
}

func (entry AuthEntry) Validate() error {
	if entry.ID == "" || entry.UserID == "" || entry.At.IsZero() || !namePattern.MatchString(entry.Operation) ||
		len(entry.IPAddress) > 64 || entry.IPAddress != strings.TrimSpace(entry.IPAddress) {
		return ErrInvalid
	}
	return nil
}
