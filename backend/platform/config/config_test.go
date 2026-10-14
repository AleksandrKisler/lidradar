package config

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey: string(EnvironmentStaging),
		databaseURLKey: "postgres://lidradar@example/lidradar",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if configuration.Environment != EnvironmentStaging {
		t.Fatalf("Load().Environment = %q, want %q", configuration.Environment, EnvironmentStaging)
	}
	if configuration.HTTP.Address != defaultHTTPAddress {
		t.Fatalf("Load().HTTP.Address = %q", configuration.HTTP.Address)
	}
	if configuration.Database.URL == "" {
		t.Fatal("Load().Database.URL is empty")
	}
}

func TestLoadRejectsMissingCriticalConfiguration(t *testing.T) {
	_, err := Load(mapLookup(nil))
	if err == nil || !strings.Contains(err.Error(), environmentKey) {
		t.Fatalf("Load() error = %v, want missing %s error", err, environmentKey)
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	_, err := Load(mapLookup(map[string]string{environmentKey: "preview"}))
	if err == nil || !strings.Contains(err.Error(), "unsupported value") {
		t.Fatalf("Load() error = %v, want unsupported value error", err)
	}
}

func TestLoadAllowsDatabaseFreeAINodeConfiguration(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{environmentKey: string(EnvironmentProduction)}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if configuration.Database.URL != "" {
		t.Fatalf("Load().Database.URL = %q, want empty", configuration.Database.URL)
	}
}

func TestLoadParsesRuntimeSettings(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey:      string(EnvironmentTest),
		httpAddressKey:      "127.0.0.1:9090",
		shutdownTimeoutKey:  "3s",
		databaseMaxConnsKey: "20",
		databaseMinConnsKey: "2",
		databaseTimeoutKey:  "750ms",
		httpRateLimitKey:    "60",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if configuration.HTTP.Address != "127.0.0.1:9090" || configuration.HTTP.ShutdownTimeout != 3*time.Second {
		t.Fatalf("Load().HTTP = %#v", configuration.HTTP)
	}
	if configuration.HTTP.RateLimitPerMinute != 60 || configuration.HTTP.WebhookRateLimitPerMinute != 1200 {
		t.Fatalf("Load().HTTP пределы частоты = %#v", configuration.HTTP)
	}
	if configuration.Database.MaxConnections != 20 || configuration.Database.MinConnections != 2 || configuration.Database.ConnectTimeout != 750*time.Millisecond {
		t.Fatalf("Load().Database = %#v", configuration.Database)
	}
}

func TestLoadRejectsInvalidRuntimeSettings(t *testing.T) {
	values := map[string]string{environmentKey: string(EnvironmentTest), databaseMaxConnsKey: "not-a-number"}
	if _, err := Load(mapLookup(values)); err == nil || !strings.Contains(err.Error(), databaseMaxConnsKey) {
		t.Fatalf("Load() error = %v, want connection limit error", err)
	}
	values = map[string]string{environmentKey: string(EnvironmentTest), httpWebhookRateLimitKey: "-1"}
	if _, err := Load(mapLookup(values)); err == nil || !strings.Contains(err.Error(), httpWebhookRateLimitKey) {
		t.Fatalf("Load() error = %v, want webhook rate limit error", err)
	}
	values = map[string]string{environmentKey: string(EnvironmentTest), httpAINodeRateLimitKey: "-1"}
	if _, err := Load(mapLookup(values)); err == nil || !strings.Contains(err.Error(), httpAINodeRateLimitKey) {
		t.Fatalf("Load() error = %v, want AI node rate limit error", err)
	}
}

// Переменная токена Telegram переименована в LIDRADAR_TELEGRAM_TOKEN; прежнее
// имя LIDAR_TELEGRAM_TOKEN принимается, новое имеет приоритет.
func TestLoadAcceptsLegacyTelegramTokenKeyAndAINodeRateLimitDefault(t *testing.T) {
	const legacyToken = "12345:abcdefghijklmnopqrstuvwxyzABCDE"
	const currentToken = "12345:abcdefghijklmnopqrstuvwxyzABCDEF"
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey:         string(EnvironmentDevelopment),
		telegramTokenLegacyKey: legacyToken,
	}))
	if err != nil || configuration.Notifications.TelegramBotToken != legacyToken {
		t.Fatalf("прежнее имя переменной токена не принято: %v", err)
	}
	configuration, err = Load(mapLookup(map[string]string{
		environmentKey:         string(EnvironmentDevelopment),
		telegramTokenKey:       currentToken,
		telegramTokenLegacyKey: legacyToken,
	}))
	if err != nil || configuration.Notifications.TelegramBotToken != currentToken {
		t.Fatalf("новое имя переменной должно иметь приоритет: %v", err)
	}
	if configuration.HTTP.AINodeRateLimitPerMinute != 600 {
		t.Fatalf("предел частоты API узла по умолчанию = %d, нужно 600", configuration.HTTP.AINodeRateLimitPerMinute)
	}
}

func TestLoadAuthAndOriginConfiguration(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey:    string(EnvironmentDevelopment),
		databaseURLKey:    "postgres://example",
		sessionTTLKey:     "24h",
		cookieSecureKey:   "false",
		allowedOriginsKey: "https://app.example.com/, https://app.example.com, http://localhost:5173",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if configuration.Auth.SessionTTL != 24*time.Hour || configuration.Auth.CookieSecure {
		t.Fatalf("Load().Auth = %#v", configuration.Auth)
	}
	if len(configuration.HTTP.AllowedOrigins) != 2 || configuration.HTTP.AllowedOrigins[0] != "https://app.example.com" {
		t.Fatalf("Load().HTTP.AllowedOrigins = %#v", configuration.HTTP.AllowedOrigins)
	}
}

func TestLoadRejectsInsecureProductionCookie(t *testing.T) {
	_, err := Load(mapLookup(map[string]string{
		environmentKey:  string(EnvironmentProduction),
		databaseURLKey:  "postgres://example",
		cookieSecureKey: "false",
	}))
	if err == nil || !strings.Contains(err.Error(), cookieSecureKey) {
		t.Fatalf("Load() error = %v, want secure cookie error", err)
	}
}

func TestLoadDefaultsSecureCookiesOutsideDevelopment(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey: string(EnvironmentStaging),
		databaseURLKey: "postgres://example",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !configuration.Auth.CookieSecure {
		t.Fatal("staging cookie must be Secure by default")
	}
}

// Регистрация закрыта по умолчанию там, где есть реальные клиенты (ADR 0057), и открыта
// в разработке и тестах: иначе каждый локальный стенд требовал бы выданный оператором код.
func TestLoadRegistrationModeDefaultsFollowEnvironment(t *testing.T) {
	cases := map[Environment]RegistrationMode{
		EnvironmentDevelopment: RegistrationOpen,
		EnvironmentTest:        RegistrationOpen,
		EnvironmentStaging:     RegistrationInviteOnly,
		EnvironmentProduction:  RegistrationInviteOnly,
	}
	for environment, want := range cases {
		configuration, err := Load(mapLookup(map[string]string{environmentKey: string(environment), databaseURLKey: "postgres://example"}))
		if err != nil {
			t.Fatalf("Load(%s) error = %v", environment, err)
		}
		if configuration.Auth.RegistrationMode != want {
			t.Errorf("Load(%s).Auth.RegistrationMode = %q, want %q", environment, configuration.Auth.RegistrationMode, want)
		}
	}
}

func TestLoadRegistrationModeIsExplicitAndValidated(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey: string(EnvironmentProduction), databaseURLKey: "postgres://example", registrationModeKey: " Open ",
	}))
	if err != nil || configuration.Auth.RegistrationMode != RegistrationOpen {
		t.Fatalf("explicit open in production: mode = %q, error = %v", configuration.Auth.RegistrationMode, err)
	}
	configuration, err = Load(mapLookup(map[string]string{
		environmentKey: string(EnvironmentDevelopment), registrationModeKey: "invite_only",
	}))
	if err != nil || configuration.Auth.RegistrationMode != RegistrationInviteOnly {
		t.Fatalf("explicit invite_only in development: mode = %q, error = %v", configuration.Auth.RegistrationMode, err)
	}
	for _, value := range []string{"closed", "invite-only", "true", ""} {
		_, err := Load(mapLookup(map[string]string{environmentKey: string(EnvironmentDevelopment), registrationModeKey: value}))
		if value == "" {
			// Пустое значение — ошибка, а не молчаливое «открыто»: пропуск настройки не должен открывать регистрацию.
			if err == nil || !strings.Contains(err.Error(), registrationModeKey) {
				t.Errorf("empty value must be rejected, error = %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), registrationModeKey) {
			t.Errorf("Load(%q) error = %v, want registration mode error", value, err)
		}
	}
}

// В staging и production API обязан получить всё, без чего веб-клиент и Telegram
// не заработают, и иначе отказаться от запуска. За прокси, завершающим TLS, API
// видит http и не узнаёт свой origin по схеме: каждая мутация браузера, включая
// вход, получила бы 403. Без публичного адреса и ключа шифрования подключение
// Telegram отвечает 503 уже после «успешного» старта. Требования относятся
// только к API; Load их не предъявляет, потому что worker, scheduler и migrate
// запускаются в тех же окружениях без этих настроек.
func TestValidateAPIRequiresBrowserAndTelegramSettingsOutsideDevelopment(t *testing.T) {
	complete := map[string]string{
		databaseURLKey:    "postgres://example",
		allowedOriginsKey: "https://app.example.com",
		publicBaseURLKey:  "https://app.example.com",
		trustedProxiesKey: "172.16.0.0/12",
		credentialKeyKey:  base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	}
	// Пустое значение удаляет ключ, остальные заменяют его.
	load := func(environment Environment, overrides map[string]string) Config {
		t.Helper()
		values := map[string]string{environmentKey: string(environment)}
		for key, value := range complete {
			values[key] = value
		}
		for key, value := range overrides {
			if value == "" {
				delete(values, key)
			} else {
				values[key] = value
			}
		}
		configuration, err := Load(mapLookup(values))
		if err != nil {
			t.Fatalf("Load(%s, %v) error = %v", environment, overrides, err)
		}
		return configuration
	}
	cases := map[string]struct {
		overrides map[string]string
		mentions  []string
	}{
		"без origins":              {map[string]string{allowedOriginsKey: ""}, []string{allowedOriginsKey}},
		"пустой список origins":    {map[string]string{allowedOriginsKey: " , "}, []string{allowedOriginsKey}},
		"без адреса и ключа":       {map[string]string{publicBaseURLKey: "", credentialKeyKey: ""}, []string{publicBaseURLKey, credentialKeyKey}},
		"без доверенных proxy":     {map[string]string{trustedProxiesKey: ""}, []string{trustedProxiesKey}},
		"список proxy из пробелов": {map[string]string{trustedProxiesKey: " , "}, []string{trustedProxiesKey}},
		"ничего не задано": {
			map[string]string{allowedOriginsKey: "", publicBaseURLKey: "", credentialKeyKey: "", trustedProxiesKey: ""},
			[]string{allowedOriginsKey, publicBaseURLKey, credentialKeyKey, trustedProxiesKey},
		},
	}
	for _, environment := range []Environment{EnvironmentStaging, EnvironmentProduction} {
		if err := load(environment, nil).ValidateAPI(); err != nil {
			t.Fatalf("%s с полным набором: ValidateAPI() error = %v", environment, err)
		}
		for name, c := range cases {
			err := load(environment, c.overrides).ValidateAPI()
			if err == nil || !strings.Contains(err.Error(), string(environment)) {
				t.Fatalf("%s, %s: ValidateAPI() error = %v", environment, name, err)
			}
			for _, key := range c.mentions {
				if !strings.Contains(err.Error(), key) {
					t.Fatalf("%s, %s: в ошибке нет %s: %v", environment, name, key, err)
				}
			}
		}
	}
	empty := map[string]string{allowedOriginsKey: "", publicBaseURLKey: "", credentialKeyKey: "", trustedProxiesKey: ""}
	for _, environment := range []Environment{EnvironmentDevelopment, EnvironmentTest} {
		if err := load(environment, empty).ValidateAPI(); err != nil {
			t.Fatalf("%s без настроек: ValidateAPI() error = %v", environment, err)
		}
	}
}

// ADR 0049: список доверенных proxy — адреса и сети; одиночный адрес становится
// узлом, IPv4 внутри IPv6 приводится к IPv4, повторы схлопываются.
func TestLoadParsesTrustedProxies(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey:    string(EnvironmentTest),
		trustedProxiesKey: " 10.0.0.0/8, 192.168.1.7 ,2001:db8:f::/48, ::1, 10.0.0.0/8 , ::ffff:172.16.0.0/108,, fc00::/7 ",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	var got []string
	for _, prefix := range configuration.HTTP.TrustedProxies {
		got = append(got, prefix.String())
	}
	want := []string{"10.0.0.0/8", "192.168.1.7/32", "2001:db8:f::/48", "::1/128", "172.16.0.0/12", "fc00::/7"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("TrustedProxies = %v, want %v", got, want)
	}
	empty, err := Load(mapLookup(map[string]string{environmentKey: string(EnvironmentTest), trustedProxiesKey: "  "}))
	if err != nil || len(empty.HTTP.TrustedProxies) != 0 {
		t.Fatalf("пустое значение: %v, %v", empty.HTTP.TrustedProxies, err)
	}
}

// Опасные и ошибочные записи отвергаются при загрузке, а не молча расширяют
// доверие: сеть «весь интернет», опечатка с битами узла, не адрес.
func TestLoadRejectsUnsafeOrInvalidTrustedProxies(t *testing.T) {
	for name, value := range map[string]string{
		"весь IPv4":                   "0.0.0.0/0",
		"весь IPv6":                   "::/0",
		"весь IPv4 внутри IPv6":       "::ffff:0.0.0.0/96",
		"биты узла в сети":            "10.0.0.1/8",
		"длина шире адреса":           "10.0.0.0/33",
		"не адрес":                    "proxy.example.com",
		"имя с маской":                "internal/24",
		"зона IPv6":                   "fe80::1%eth0",
		"IPv4 внутри IPv6 шире /96":   "::ffff:0.0.0.0/80",
		"плохая запись среди хороших": "10.0.0.0/8, nonsense",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(mapLookup(map[string]string{environmentKey: string(EnvironmentTest), trustedProxiesKey: value}))
			if err == nil || !strings.Contains(err.Error(), trustedProxiesKey) {
				t.Fatalf("Load(%q) error = %v, want error naming %s", value, err, trustedProxiesKey)
			}
		})
	}
	_, err := Load(mapLookup(map[string]string{environmentKey: string(EnvironmentTest), trustedProxiesKey: "0.0.0.0/0"}))
	if err == nil || !strings.Contains(err.Error(), "every address") {
		t.Fatalf("сообщение о «весь интернет»: %v", err)
	}
	_, err = Load(mapLookup(map[string]string{environmentKey: string(EnvironmentTest), trustedProxiesKey: "10.0.0.1/8"}))
	if err == nil || !strings.Contains(err.Error(), "10.0.0.0/8") {
		t.Fatalf("подсказка про корректную запись: %v", err)
	}
}

// Подключение к PostgreSQL в staging и production обязано быть зашифровано;
// открытый канал принимается только по явному LIDRADAR_DATABASE_ALLOW_PLAINTEXT.
func TestLoadRequiresEncryptedDatabaseOutsideDevelopment(t *testing.T) {
	load := func(environment Environment, allowPlaintext string) Config {
		t.Helper()
		values := map[string]string{environmentKey: string(environment), databaseURLKey: "postgres://example"}
		if allowPlaintext != "" {
			values[databasePlaintextKey] = allowPlaintext
		}
		configuration, err := Load(mapLookup(values))
		if err != nil {
			t.Fatalf("Load(%s, %q) error = %v", environment, allowPlaintext, err)
		}
		return configuration
	}
	for environment, want := range map[Environment]bool{
		EnvironmentDevelopment: false, EnvironmentTest: false, EnvironmentStaging: true, EnvironmentProduction: true,
	} {
		if got := load(environment, "").Database.RequireTLS; got != want {
			t.Fatalf("%s: RequireTLS = %v, want %v", environment, got, want)
		}
	}
	for _, environment := range []Environment{EnvironmentStaging, EnvironmentProduction} {
		if load(environment, "true").Database.RequireTLS {
			t.Fatalf("%s: явное разрешение открытого канала не снимает требование", environment)
		}
		if !load(environment, "false").Database.RequireTLS {
			t.Fatalf("%s: false не должен снимать требование", environment)
		}
	}
	_, err := Load(mapLookup(map[string]string{
		environmentKey: string(EnvironmentProduction), databaseURLKey: "postgres://example", databasePlaintextKey: "maybe",
	}))
	if err == nil || !strings.Contains(err.Error(), databasePlaintextKey) {
		t.Fatalf("Load() error = %v, want boolean error for %s", err, databasePlaintextKey)
	}
}

func TestLoadRejectsInvalidAllowedOrigin(t *testing.T) {
	_, err := Load(mapLookup(map[string]string{
		environmentKey:    string(EnvironmentTest),
		allowedOriginsKey: "javascript:alert(1)",
	}))
	if err == nil || !strings.Contains(err.Error(), allowedOriginsKey) {
		t.Fatalf("Load() error = %v, want allowed origin error", err)
	}
}

func TestLoadRejectsNilLookup(t *testing.T) {
	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want lookup error")
	}
}

func TestLoadTelegramIntegrationConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey:   string(EnvironmentDevelopment),
		publicBaseURLKey: "https://telegram-dev.example.com/",
		credentialKeyKey: key,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Integrations.PublicBaseURL != "https://telegram-dev.example.com" ||
		len(configuration.Integrations.CredentialKey) != 32 {
		t.Fatalf("Integrations = %#v", configuration.Integrations)
	}
}

func TestLoadRejectsPartialOrUnsafeTelegramConfiguration(t *testing.T) {
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	for name, values := range map[string]map[string]string{
		"только адрес":  {environmentKey: string(EnvironmentDevelopment), publicBaseURLKey: "https://example.com"},
		"только ключ":   {environmentKey: string(EnvironmentDevelopment), credentialKeyKey: key},
		"без HTTPS":     {environmentKey: string(EnvironmentDevelopment), publicBaseURLKey: "http://example.com", credentialKeyKey: key},
		"короткий ключ": {environmentKey: string(EnvironmentDevelopment), publicBaseURLKey: "https://example.com", credentialKeyKey: base64.StdEncoding.EncodeToString([]byte("short"))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(mapLookup(values)); err == nil {
				t.Fatal("Load() не отклонил небезопасную конфигурацию")
			}
		})
	}
}

func TestLoadNotificationTelegramConfigurationDoesNotExposeInvalidToken(t *testing.T) {
	const invalidToken = "secret-token-that-must-not-appear"
	_, err := Load(mapLookup(map[string]string{
		environmentKey:   string(EnvironmentDevelopment),
		telegramTokenKey: invalidToken,
	}))
	if err == nil || !strings.Contains(err.Error(), telegramTokenKey) || strings.Contains(err.Error(), invalidToken) {
		t.Fatalf("безопасная ошибка Telegram token = %v", err)
	}
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey:      string(EnvironmentDevelopment),
		telegramTokenKey:    "12345:abcdefghijklmnopqrstuvwxyzABCDE",
		telegramUsernameKey: "LidRadarDevBot",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Notifications.TelegramUsername != "LidRadarDevBot" ||
		configuration.Notifications.TelegramBotToken == "" {
		t.Fatal("настройки Telegram-уведомлений не загружены")
	}
}

func TestLoadAIConfiguration(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey:       string(EnvironmentDevelopment),
		aiCloudURLKey:        "http://127.0.0.1:8080/",
		aiCredentialsKey:     "runtime/node.json",
		aiProviderKey:        "llama",
		aiLlamaURLKey:        "http://llama-server:8080/v1/chat/completions",
		aiModelVersionKey:    "candidate-q4",
		aiPollIntervalKey:    "2s",
		aiHeartbeatKey:       "9s",
		aiHTTPTimeoutKey:     "3m",
		aiSignatureWindowKey: "45s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.AI.CloudURL != "http://127.0.0.1:8080" ||
		configuration.AI.Provider != "llama" || configuration.AI.PollInterval != 2*time.Second ||
		configuration.AI.HeartbeatInterval != 9*time.Second || configuration.AI.SignatureWindow != 45*time.Second {
		t.Fatalf("AI = %#v", configuration.AI)
	}
}

func TestLoadRejectsUnsafeAIConfiguration(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"неизвестный поставщик": {
			environmentKey: string(EnvironmentDevelopment), aiProviderKey: "unknown",
		},
		"небезопасный cloud в production": {
			environmentKey: string(EnvironmentProduction), aiCloudURLKey: "http://ai.example.com",
		},
		"относительный secret в production": {
			environmentKey: string(EnvironmentProduction), aiCloudURLKey: "https://ai.example.com",
			aiCredentialsKey: "node.json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(mapLookup(values)); err == nil {
				t.Fatal("unsafe AI configuration was accepted")
			}
		})
	}
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

// Настройки почты (ADR 0058): умолчания разработки, адрес ссылок и защита соединения.
func TestLoadMailDefaults(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{environmentKey: string(EnvironmentDevelopment)}))
	if err != nil {
		t.Fatal(err)
	}
	mail := configuration.Mail
	if mail.WebBaseURL != "http://localhost:5173" || mail.FromName != "LidRadar" || mail.From != "" ||
		mail.SMTP.Host != "" || mail.SMTP.Port != 587 || mail.SMTP.Security != SMTPStartTLS {
		t.Fatalf("Mail = %#v", mail)
	}
	// Без SMTP-хоста процессу, которому почта не нужна, настройка не мешает.
	if err := configuration.ValidateWorker(); err != nil || configuration.ValidateAPI() != nil {
		t.Fatalf("development must start without mail: %v", err)
	}
}

func TestLoadMailWebBaseURLFollowsPublicBaseURLAndCanBeOverridden(t *testing.T) {
	base := map[string]string{
		environmentKey: string(EnvironmentStaging), databaseURLKey: "postgres://example",
		publicBaseURLKey: "https://app.example.com/",
		credentialKeyKey: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	}
	configuration, err := Load(mapLookup(base))
	if err != nil || configuration.Mail.WebBaseURL != "https://app.example.com" {
		t.Fatalf("WebBaseURL = %q, error = %v", configuration.Mail.WebBaseURL, err)
	}
	base[webBaseURLKey] = " https://web.example.com/ "
	configuration, err = Load(mapLookup(base))
	if err != nil || configuration.Mail.WebBaseURL != "https://web.example.com" {
		t.Fatalf("explicit WebBaseURL = %q, error = %v", configuration.Mail.WebBaseURL, err)
	}
	// Ссылки в письмах с токеном не должны уходить по открытому каналу, а адрес — нести путь или учётные данные.
	for name, value := range map[string]string{
		"http в staging": "http://web.example.com", "путь": "https://web.example.com/app", "запрос": "https://web.example.com?x=1",
		"фрагмент": "https://web.example.com#a", "учётные данные": "https://user:pass@web.example.com", "схема": "ftp://web.example.com",
		"без хоста": "https://",
	} {
		base[webBaseURLKey] = value
		if _, err := Load(mapLookup(base)); err == nil || !strings.Contains(err.Error(), webBaseURLKey) {
			t.Errorf("%s: Load() error = %v", name, err)
		}
	}
	// В разработке http допустим.
	if _, err := Load(mapLookup(map[string]string{environmentKey: "development", webBaseURLKey: "http://localhost:5173"})); err != nil {
		t.Fatalf("http in development: %v", err)
	}
}

func TestLoadMailSMTPSecurityAndPort(t *testing.T) {
	load := func(values map[string]string) (Config, error) {
		values[environmentKey] = string(EnvironmentDevelopment)
		return Load(mapLookup(values))
	}
	configuration, err := load(map[string]string{mailSMTPPortKey: "465"})
	if err != nil || configuration.Mail.SMTP.Security != SMTPImplicitTLS || configuration.Mail.SMTP.Port != 465 {
		t.Fatalf("порт 465 должен включать шифрование с первого байта: %#v, %v", configuration.Mail.SMTP, err)
	}
	configuration, err = load(map[string]string{mailSMTPPortKey: "465", mailSMTPSecurityKey: " STARTTLS "})
	if err != nil || configuration.Mail.SMTP.Security != SMTPStartTLS {
		t.Fatalf("явная защита важнее порта: %#v, %v", configuration.Mail.SMTP, err)
	}
	for name, values := range map[string]map[string]string{
		"неизвестная защита": {mailSMTPSecurityKey: "ssl"},
		"пустая защита":      {mailSMTPSecurityKey: ""},
		"порт не число":      {mailSMTPPortKey: "abc"},
		"порт ноль":          {mailSMTPPortKey: "0"},
		"порт выше предела":  {mailSMTPPortKey: "65536"},
		"порт отрицательный": {mailSMTPPortKey: "-1"},
	} {
		if _, err := load(values); err == nil || !strings.Contains(err.Error(), "LIDRADAR_MAIL_SMTP") {
			t.Errorf("%s: Load() error = %v", name, err)
		}
	}
}

func TestLoadMailRejectsUnsafeOrInconsistentSMTPSettings(t *testing.T) {
	valid := map[string]string{
		mailSMTPHostKey: "smtp.gmail.com", mailSMTPUsernameKey: "bot@gmail.com", mailSMTPPasswordKey: "app-password",
		mailFromKey: "bot@gmail.com",
	}
	load := func(environment Environment, overrides map[string]string) (Config, error) {
		values := map[string]string{environmentKey: string(environment), databaseURLKey: "postgres://example"}
		for key, value := range valid {
			values[key] = value
		}
		for key, value := range overrides {
			if value == "-" {
				delete(values, key)
			} else {
				values[key] = value
			}
		}
		return Load(mapLookup(values))
	}
	if configuration, err := load(EnvironmentProduction, nil); err != nil {
		t.Fatalf("a complete set must load: %v", err)
	} else if err := configuration.ValidateWorker(); err != nil {
		t.Fatalf("a complete set must satisfy the worker: %v", err)
	}
	cases := map[string]map[string]string{
		"имя пользователя без пароля":        {mailSMTPPasswordKey: "-"},
		"пароль без имени пользователя":      {mailSMTPUsernameKey: "-"},
		"отправитель не задан":               {mailFromKey: "-"},
		"отправитель с именем":               {mailFromKey: "Bot <bot@gmail.com>"},
		"отправитель не адрес":               {mailFromKey: "bot"},
		"хост со схемой":                     {mailSMTPHostKey: "smtp://smtp.gmail.com"},
		"хост с портом":                      {mailSMTPHostKey: "smtp.gmail.com:587"},
		"хост с пробелом":                    {mailSMTPHostKey: "smtp gmail.com"},
		"открытый канал к внешнему хосту":    {mailSMTPSecurityKey: "none"},
		"имя отправителя с переводом строки": {mailFromNameKey: "Lid\nRadar"},
		"имя отправителя слишком длинное":    {mailFromNameKey: strings.Repeat("a", 101)},
	}
	for name, overrides := range cases {
		if _, err := load(EnvironmentProduction, overrides); err == nil || !strings.Contains(err.Error(), "LIDRADAR_MAIL") {
			t.Errorf("%s: Load() error = %v", name, err)
		}
	}
	// Реквизиты без хоста — ошибка, а не молчаливое «почты нет».
	if _, err := load(EnvironmentDevelopment, map[string]string{mailSMTPHostKey: "-"}); err == nil || !strings.Contains(err.Error(), mailSMTPHostKey) {
		t.Errorf("credentials without a host: error = %v", err)
	}
}

func TestLoadMailPlainSMTPIsOnlyForLoopbackOutsideProduction(t *testing.T) {
	plain := func(environment Environment, host string) error {
		_, err := Load(mapLookup(map[string]string{
			environmentKey: string(environment), databaseURLKey: "postgres://example", mailSMTPHostKey: host,
			mailSMTPPortKey: "1025", mailSMTPSecurityKey: "none", mailFromKey: "bot@example.com",
		}))
		return err
	}
	for _, host := range []string{"localhost", "127.0.0.1", "::1", "[::1]"} {
		if err := plain(EnvironmentDevelopment, host); err != nil {
			t.Errorf("loopback %s in development: %v", host, err)
		}
		if err := plain(EnvironmentProduction, host); err == nil {
			t.Errorf("loopback %s must still be refused in production", host)
		}
	}
	for _, host := range []string{"smtp.example.com", "192.0.2.10", "0.0.0.0"} {
		if err := plain(EnvironmentDevelopment, host); err == nil {
			t.Errorf("plain SMTP to %s must be refused", host)
		}
	}
}

// Worker в staging и production не стартует без почты: журнальный транспорт с токенами там недопустим, а без
// SMTP сброс пароля молча не работал бы.
func TestValidateWorkerRequiresMailOutsideDevelopment(t *testing.T) {
	complete := map[string]string{
		databaseURLKey: "postgres://example", mailSMTPHostKey: "smtp.gmail.com", mailSMTPUsernameKey: "bot@gmail.com",
		mailSMTPPasswordKey: "app-password", mailFromKey: "bot@gmail.com",
	}
	load := func(environment Environment, drop ...string) Config {
		t.Helper()
		values := map[string]string{environmentKey: string(environment)}
		for key, value := range complete {
			values[key] = value
		}
		for _, key := range drop {
			delete(values, key)
		}
		configuration, err := Load(mapLookup(values))
		if err != nil {
			t.Fatalf("Load(%s, drop %v) error = %v", environment, drop, err)
		}
		return configuration
	}
	for _, environment := range []Environment{EnvironmentStaging, EnvironmentProduction} {
		if err := load(environment).ValidateWorker(); err != nil {
			t.Fatalf("%s с полным набором: %v", environment, err)
		}
		// Хост без отправителя отвергает уже сама загрузка: письмо без адреса отправителя уйти не может.
		values := map[string]string{environmentKey: string(environment)}
		for key, value := range complete {
			values[key] = value
		}
		delete(values, mailFromKey)
		if _, err := Load(mapLookup(values)); err == nil || !strings.Contains(err.Error(), mailFromKey) {
			t.Errorf("%s без отправителя: Load() error = %v", environment, err)
		}
		for name, drop := range map[string][]string{
			"без хоста": {mailSMTPHostKey, mailSMTPUsernameKey, mailSMTPPasswordKey, mailFromKey},
			"без входа": {mailSMTPUsernameKey, mailSMTPPasswordKey},
		} {
			err := load(environment, drop...).ValidateWorker()
			if err == nil || !strings.Contains(err.Error(), string(environment)) {
				t.Errorf("%s, %s: ValidateWorker() error = %v", environment, name, err)
			}
		}
	}
	for _, environment := range []Environment{EnvironmentDevelopment, EnvironmentTest} {
		if err := load(environment, mailSMTPHostKey, mailSMTPUsernameKey, mailSMTPPasswordKey, mailFromKey).ValidateWorker(); err != nil {
			t.Errorf("%s без почты: %v", environment, err)
		}
	}
}

func TestValidateAPIRequiresALinkBase(t *testing.T) {
	values := map[string]string{
		environmentKey: string(EnvironmentProduction), databaseURLKey: "postgres://example",
		allowedOriginsKey: "https://app.example.com", trustedProxiesKey: "172.16.0.0/12",
		credentialKeyKey: base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
		publicBaseURLKey: "https://app.example.com",
	}
	configuration, err := Load(mapLookup(values))
	if err != nil || configuration.ValidateAPI() != nil {
		t.Fatalf("a complete API configuration must pass: %v / %v", err, configuration.ValidateAPI())
	}
	configuration.Mail.WebBaseURL = ""
	if err := configuration.ValidateAPI(); err == nil || !strings.Contains(err.Error(), webBaseURLKey) {
		t.Fatalf("an empty link base must be reported: %v", err)
	}
}

// Пароль SMTP не должен попадать в журнал даже при печати всей конфигурации.
func TestSMTPSettingsNeverPrintThePassword(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey: string(EnvironmentDevelopment), mailSMTPHostKey: "smtp.gmail.com", mailSMTPUsernameKey: "bot@gmail.com",
		mailSMTPPasswordKey: "super-secret-app-password", mailFromKey: "bot@gmail.com",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{
		fmt.Sprintf("%v", configuration), fmt.Sprintf("%+v", configuration), fmt.Sprintf("%#v", configuration),
		fmt.Sprintf("%v", configuration.Mail.SMTP), fmt.Sprint(configuration.Mail.SMTP), fmt.Sprintf("%#v", configuration.Mail.SMTP),
	} {
		if strings.Contains(rendered, "super-secret-app-password") {
			t.Fatalf("пароль SMTP напечатан: %s", rendered)
		}
	}
	if !strings.Contains(configuration.Mail.SMTP.String(), "[redacted]") || !strings.Contains(configuration.Mail.SMTP.String(), "smtp.gmail.com") {
		t.Fatalf("String() = %s", configuration.Mail.SMTP.String())
	}
	if (SMTP{}).String() == "" || strings.Contains((SMTP{}).String(), "[redacted]") {
		t.Fatalf("пустой пароль не должен выглядеть заданным: %s", (SMTP{}).String())
	}
}
