package domain

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrInvalid = errors.New("invalid lifecycle request")
	// ErrNotFound: пользователь или организация не найдены.
	ErrNotFound = errors.New("lifecycle subject not found")
)

const (
	// PersonalExportFormat и OrganizationExportFormat — версии форматов выгрузок; меняются при несовместимых
	// изменениях состава.
	PersonalExportFormat     = "lidradar.account-export/1"
	OrganizationExportFormat = "lidradar.organization-export/1"

	// SectionLimit — сколько строк личной выгрузки отдаёт один список; при превышении раздел помечается как
	// обрезанный, а не обрывается молча.
	SectionLimit = 50000

	// ExportTimeout — предел времени выгрузки организации целиком; MaxConcurrentExports — сколько выгрузок
	// организаций идёт в одном процессе одновременно.
	ExportTimeout        = 15 * time.Minute
	MaxConcurrentExports = 2
)

// Section — список личной выгрузки. Truncated истинно, если строк было больше SectionLimit.
type Section[T any] struct {
	Items     []T  `json:"items"`
	Truncated bool `json:"truncated"`
}

// MarshalJSON кодирует пустой список как [], а не null: читателю файла не нужно различать два вида пустоты.
func (section Section[T]) MarshalJSON() ([]byte, error) {
	items := section.Items
	if items == nil {
		items = []T{}
	}
	return json.Marshal(struct {
		Items     []T  `json:"items"`
		Truncated bool `json:"truncated"`
	}{Items: items, Truncated: section.Truncated})
}

// PersonalExport — документ личной выгрузки (формат lidradar.account-export/1). Тег json задаёт имена полей файла.
type PersonalExport struct {
	Format      string    `json:"format"`
	GeneratedAt time.Time `json:"generatedAt"`
	// Note напоминает, что содержимое внесённых записей принадлежит организации и выгружается её владельцем.
	Note                 string                          `json:"note"`
	Account              ExportAccount                   `json:"account"`
	Memberships          Section[ExportMembership]       `json:"memberships"`
	Sessions             Section[ExportSession]          `json:"sessions"`
	AuthEvents           Section[ExportAuthEvent]        `json:"authEvents"`
	NotificationSettings Section[ExportNotificationRule] `json:"notificationSettings"`
	TelegramLinks        Section[ExportTelegramLink]     `json:"telegramLinks"`
	ConsentDecisions     Section[ExportConsentDecision]  `json:"consentDecisions"`
	AuthoredRecords      []ExportAuthored                `json:"authoredRecords"`
}

// PersonalExportNote — постоянное пояснение к выгрузке.
const PersonalExportNote = "Содержимое записей, которые вы вносили в организациях (заметки, вердикты, сделки, переписки), " +
	"принадлежит организации и выгружается её владельцем. Здесь — данные вашей учётной записи и счётчики внесённых записей."

type ExportAccount struct {
	ID                  string     `json:"id"`
	Email               string     `json:"email"`
	DisplayName         string     `json:"displayName"`
	Status              string     `json:"status"`
	CreatedAt           time.Time  `json:"createdAt"`
	EmailVerifiedAt     *time.Time `json:"emailVerifiedAt"`
	DeletionRequestedAt *time.Time `json:"deletionRequestedAt"`
	DeletionDueAt       *time.Time `json:"deletionDueAt"`
}

type ExportMembership struct {
	Organization string     `json:"organization"`
	Role         string     `json:"role"`
	Status       string     `json:"status"`
	JoinedAt     time.Time  `json:"joinedAt"`
	RevokedAt    *time.Time `json:"revokedAt"`
}

type ExportSession struct {
	CreatedAt time.Time  `json:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"`
	RevokedAt *time.Time `json:"revokedAt"`
	IPAddress string     `json:"ipAddress,omitempty"`
	UserAgent string     `json:"userAgent,omitempty"`
}

type ExportAuthEvent struct {
	Operation string    `json:"operation"`
	IPAddress string    `json:"ipAddress,omitempty"`
	At        time.Time `json:"at"`
}

type ExportNotificationRule struct {
	Organization      string  `json:"organization"`
	RiskType          string  `json:"riskType"`
	MinimumSeverity   string  `json:"minimumSeverity"`
	DeliveryMode      string  `json:"deliveryMode"`
	InAppEnabled      bool    `json:"inAppEnabled"`
	TelegramEnabled   bool    `json:"telegramEnabled"`
	QuietHoursEnabled bool    `json:"quietHoursEnabled"`
	QuietHoursStart   *string `json:"quietHoursStart"`
	QuietHoursEnd     *string `json:"quietHoursEnd"`
	DigestTime        *string `json:"digestTime"`
}

type ExportTelegramLink struct {
	Organization   string     `json:"organization"`
	TelegramUserID int64      `json:"telegramUserId"`
	ChatID         int64      `json:"chatId"`
	LinkedAt       time.Time  `json:"linkedAt"`
	DisabledAt     *time.Time `json:"disabledAt"`
}

// ExportConsentDecision — решение по согласию на наборы данных, принятое этим пользователем.
type ExportConsentDecision struct {
	Organization string    `json:"organization"`
	Scope        string    `json:"scope"`
	Decision     string    `json:"decision"` // GRANTED или REVOKED
	At           time.Time `json:"at"`
}

// ExportAuthored — сколько записей пользователь внёс в организации. Содержимое записей в личную выгрузку не входит.
type ExportAuthored struct {
	Organization         string `json:"organization"`
	Actions              int64  `json:"actions"`
	Outcomes             int64  `json:"outcomes"`
	RiskFeedback         int64  `json:"riskFeedback"`
	RevenueConfirmations int64  `json:"revenueConfirmations"`
	StageChanges         int64  `json:"stageChanges"`
	InvitationsCreated   int64  `json:"invitationsCreated"`
	AuditEvents          int64  `json:"auditEvents"`
}

// ManifestFile описывает один файл архива выгрузки организации.
type ManifestFile struct {
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Manifest — последний файл архива. Архив без манифеста оборван и недействителен.
type Manifest struct {
	Format      string         `json:"format"`
	GeneratedAt time.Time      `json:"generatedAt"`
	Files       []ManifestFile `json:"files"`
}
