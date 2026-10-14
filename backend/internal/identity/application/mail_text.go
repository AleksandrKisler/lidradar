package application

import "time"

// Тексты писем учётной записи (ADR 0058). Обычный текст без имени получателя: в письме нет
// персональных данных, кроме ссылки с токеном. Срок действия в тексте совпадает с passwordResetTTL
// и emailVerificationTTL; тест проверяет это соответствие.

const (
	mailKindEmailVerification = "EMAIL_VERIFICATION"
	mailKindPasswordReset     = "PASSWORD_RESET"
	mailKindPasswordChanged   = "PASSWORD_CHANGED"

	// Уведомления о закрытии учётной записи (ADR 0059). Письма об исполнении и о блокировке при исполнении
	// пишет модуль lifecycle.
	mailKindAccountDeletionRequested = "ACCOUNT_DELETION_REQUESTED"
	mailKindAccountDeletionCancelled = "ACCOUNT_DELETION_CANCELLED"
)

func emailVerificationMail(link string) (subject, text string) {
	return "Подтвердите почту в LidRadar",
		"Здравствуйте!\n\n" +
			"Подтвердите адрес почты для LidRadar по ссылке:\n\n" +
			link + "\n\n" +
			"Ссылка действует 7 суток. Если вы не регистрировались в LidRadar, просто проигнорируйте это письмо.\n"
}

func passwordResetMail(link string) (subject, text string) {
	return "Сброс пароля LidRadar",
		"Здравствуйте!\n\n" +
			"Чтобы задать новый пароль в LidRadar, откройте ссылку:\n\n" +
			link + "\n\n" +
			"Ссылка действует 60 минут и работает один раз. Если вы не запрашивали сброс, ничего делать не нужно: пароль останется прежним.\n"
}

func passwordChangedMail() (subject, text string) {
	return "Пароль LidRadar изменён",
		"Здравствуйте!\n\n" +
			"Пароль вашей учётной записи в LidRadar был изменён, остальные сеансы завершены.\n\n" +
			"Если это сделали не вы, откройте страницу входа и сбросьте пароль по ссылке «Забыли пароль?».\n"
}

// deletionRequestedMail сообщает о назначенном закрытии. Дата в UTC: у письма нет часового пояса пользователя.
func deletionRequestedMail(settingsLink, forgotLink string, due time.Time) (subject, text string) {
	return "Запрошено удаление аккаунта LidRadar",
		"Здравствуйте!\n\n" +
			"Для аккаунта LidRadar на этот адрес запрошено удаление. Данные аккаунта будут обезличены " +
			due.UTC().Format("02.01.2006 15:04") + " UTC. До этого момента вход работает как обычно.\n\n" +
			"Чтобы отменить удаление, войдите и откройте «Настройки → Аккаунт»:\n\n" +
			settingsLink + "\n\n" +
			"Если запрос отправили не вы, отмените удаление и смените пароль: на странице входа выберите «Забыли пароль?»\n" +
			forgotLink + "\n"
}

func deletionCancelledMail(forgotLink string) (subject, text string) {
	return "Удаление аккаунта LidRadar отменено",
		"Здравствуйте!\n\n" +
			"Запрошенное удаление аккаунта LidRadar отменено, данные остаются как были.\n\n" +
			"Если отменили не вы, смените пароль: на странице входа выберите «Забыли пароль?»\n" +
			forgotLink + "\n"
}
