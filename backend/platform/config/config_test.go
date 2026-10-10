package config

import (
	"encoding/base64"
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
		"без origins":           {map[string]string{allowedOriginsKey: ""}, []string{allowedOriginsKey}},
		"пустой список origins": {map[string]string{allowedOriginsKey: " , "}, []string{allowedOriginsKey}},
		"без адреса и ключа":    {map[string]string{publicBaseURLKey: "", credentialKeyKey: ""}, []string{publicBaseURLKey, credentialKeyKey}},
		"ничего не задано": {
			map[string]string{allowedOriginsKey: "", publicBaseURLKey: "", credentialKeyKey: ""},
			[]string{allowedOriginsKey, publicBaseURLKey, credentialKeyKey},
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
	empty := map[string]string{allowedOriginsKey: "", publicBaseURLKey: "", credentialKeyKey: ""}
	for _, environment := range []Environment{EnvironmentDevelopment, EnvironmentTest} {
		if err := load(environment, empty).ValidateAPI(); err != nil {
			t.Fatalf("%s без настроек: ValidateAPI() error = %v", environment, err)
		}
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
