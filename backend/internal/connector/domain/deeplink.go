package domain

import "regexp"

// DeepLinkKind — вид внешней ссылки на собеседника в клиенте поставщика.
type DeepLinkKind string

// DeepLinkTelegramUser открывает профиль пользователя Telegram по числовому
// идентификатору (`tg://user?id=…`): для бизнес-чатов владелец уже переписывался
// с этим пользователем, поэтому клиент показывает диалог.
const DeepLinkTelegramUser DeepLinkKind = "TELEGRAM_USER"

// Причины отсутствия внешней ссылки; интерфейс показывает их вместо кнопки.
const (
	DeepLinkUnavailableProvider = "PROVIDER_UNSUPPORTED"
	DeepLinkUnavailableIdentity = "IDENTITY_UNKNOWN"
)

// ContactLink — безопасная внешняя ссылка: строится только сервером из
// проверенного внешнего идентификатора контакта и разрешённой схемы.
type ContactLink struct {
	URL  string
	Kind DeepLinkKind
}

var telegramUserIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)

// ContactDeepLink возвращает ссылку на собеседника для поставщика канала либо
// код причины, почему ссылку построить нельзя. Непрозрачные идентификаторы
// никогда не подставляются в URL как есть: единственная разрешённая схема —
// `tg://user?id=<число>` для Telegram.
func ContactDeepLink(provider Provider, contactExternalID string) (ContactLink, string) {
	if provider != ProviderTelegramConnectedBusinessBot {
		return ContactLink{}, DeepLinkUnavailableProvider
	}
	if !telegramUserIDPattern.MatchString(contactExternalID) {
		return ContactLink{}, DeepLinkUnavailableIdentity
	}
	return ContactLink{URL: "tg://user?id=" + contactExternalID, Kind: DeepLinkTelegramUser}, ""
}
