package application

// Тексты писем учётной записи (ADR 0058). Обычный текст без имени получателя: в письме нет
// персональных данных, кроме ссылки с токеном. Срок действия в тексте совпадает с passwordResetTTL
// и emailVerificationTTL; тест проверяет это соответствие.

const (
	mailKindEmailVerification = "EMAIL_VERIFICATION"
	mailKindPasswordReset     = "PASSWORD_RESET"
	mailKindPasswordChanged   = "PASSWORD_CHANGED"
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
