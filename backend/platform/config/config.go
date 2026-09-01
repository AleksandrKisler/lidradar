// Package config loads and validates LidRadar runtime configuration.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	environmentKey           = "LIDRADAR_ENV"
	httpAddressKey           = "LIDRADAR_HTTP_ADDRESS"
	databaseURLKey           = "LIDRADAR_DATABASE_URL"
	shutdownTimeoutKey       = "LIDRADAR_SHUTDOWN_TIMEOUT"
	databaseMaxConnsKey      = "LIDRADAR_DATABASE_MAX_CONNS"
	databaseMinConnsKey      = "LIDRADAR_DATABASE_MIN_CONNS"
	databaseTimeoutKey       = "LIDRADAR_DATABASE_TIMEOUT"
	allowedOriginsKey        = "LIDRADAR_ALLOWED_ORIGINS"
	sessionTTLKey            = "LIDRADAR_SESSION_TTL"
	cookieSecureKey          = "LIDRADAR_COOKIE_SECURE"
	publicBaseURLKey         = "LIDRADAR_PUBLIC_BASE_URL"
	credentialKeyKey         = "LIDRADAR_INTEGRATION_ENCRYPTION_KEY"
	telegramTokenKey         = "LIDAR_TELEGRAM_TOKEN"
	telegramUsernameKey      = "LIDRADAR_TELEGRAM_BOT_USERNAME"
	aiCloudURLKey            = "LIDRADAR_AI_CLOUD_URL"
	aiCredentialsKey         = "LIDRADAR_AI_CREDENTIALS_FILE"
	aiProviderKey            = "LIDRADAR_AI_PROVIDER"
	aiLlamaURLKey            = "LIDRADAR_AI_LLAMA_URL"
	aiModelVersionKey        = "LIDRADAR_AI_MODEL_VERSION"
	aiPollIntervalKey        = "LIDRADAR_AI_POLL_INTERVAL"
	aiHeartbeatKey           = "LIDRADAR_AI_HEARTBEAT_INTERVAL"
	aiHTTPTimeoutKey         = "LIDRADAR_AI_HTTP_TIMEOUT"
	aiSignatureWindowKey     = "LIDRADAR_AI_SIGNATURE_WINDOW"
	defaultHTTPAddress       = ":8080"
	defaultDatabaseURL       = "postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable"
	defaultShutdown          = 10 * time.Second
	defaultDatabaseWait      = 5 * time.Second
	defaultSessionTTL        = 30 * 24 * time.Hour
	defaultDatabaseMax       = int32(10)
	defaultDatabaseMin       = int32(1)
	defaultTelegramBot       = "LidRadarDevBot"
	defaultAIProvider        = "fake"
	defaultAILlamaURL        = "http://llama-server:8080/v1/chat/completions"
	defaultAIModel           = "lidradar-main-v1"
	defaultAIPoll            = time.Second
	defaultAIHeartbeat       = 10 * time.Second
	defaultAIHTTPTimeout     = 5 * time.Minute
	defaultAISignatureWindow = 60 * time.Second
)

// LookupEnv is the environment lookup contract used by Load. Keeping the
// environment boundary explicit makes configuration loading deterministic in
// tests and allows commands to use os.LookupEnv without global state here.
type LookupEnv func(string) (string, bool)

// Environment identifies the deployment environment a process belongs to.
type Environment string

const (
	EnvironmentDevelopment Environment = "development"
	EnvironmentTest        Environment = "test"
	EnvironmentStaging     Environment = "staging"
	EnvironmentProduction  Environment = "production"
)

// Config contains configuration shared by every LidRadar runtime.
type Config struct {
	Environment   Environment
	HTTP          HTTP
	Database      Database
	Auth          Auth
	Integrations  Integrations
	Notifications Notifications
	AI            AI
}

// HTTP contains process-level HTTP server settings.
type HTTP struct {
	Address         string
	ShutdownTimeout time.Duration
	AllowedOrigins  []string
}

// Auth contains server-side session and cookie settings.
type Auth struct {
	SessionTTL   time.Duration
	CookieSecure bool
}

// Integrations содержит общие безопасные настройки внешних подключений.
type Integrations struct {
	PublicBaseURL string
	CredentialKey []byte
}

// Notifications содержит настройки исходящих оповещений. BotToken никогда не
// должен попадать в журналы или диагностические ответы.
type Notifications struct {
	TelegramBotToken string
	TelegramUsername string
}

// AI содержит машинные настройки Cloud Core и одноразового домашнего узла.
// Реквизиты узла читаются только из файла с ограниченными правами.
type AI struct {
	CloudURL, CredentialsFile, Provider string
	LlamaURL, ModelVersion              string
	PollInterval, HeartbeatInterval     time.Duration
	HTTPTimeout, SignatureWindow        time.Duration
}

// Database contains PostgreSQL connection-pool settings.
type Database struct {
	URL            string
	MaxConnections int32
	MinConnections int32
	ConnectTimeout time.Duration
}

// Load reads typed configuration from environment variables and validates all
// critical values before returning it.
func Load(lookup LookupEnv) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("configuration lookup is required")
	}

	rawEnvironment, present := lookup(environmentKey)
	if !present || rawEnvironment == "" {
		return Config{}, fmt.Errorf("required environment variable %s is missing", environmentKey)
	}

	var err error
	configuration := Config{
		Environment: Environment(rawEnvironment),
		HTTP: HTTP{
			Address:         valueOrDefault(lookup, httpAddressKey, defaultHTTPAddress),
			ShutdownTimeout: defaultShutdown,
			AllowedOrigins:  stringListValue(lookup, allowedOriginsKey),
		},
		Database: Database{
			URL:            valueOrDefault(lookup, databaseURLKey, ""),
			MaxConnections: defaultDatabaseMax,
			MinConnections: defaultDatabaseMin,
			ConnectTimeout: defaultDatabaseWait,
		},
		Auth: Auth{SessionTTL: defaultSessionTTL},
		Integrations: Integrations{
			PublicBaseURL: strings.TrimRight(strings.TrimSpace(valueOrDefault(lookup, publicBaseURLKey, "")), "/"),
		},
		Notifications: Notifications{
			TelegramBotToken: valueOrDefault(lookup, telegramTokenKey, ""),
			TelegramUsername: strings.TrimPrefix(strings.TrimSpace(valueOrDefault(lookup, telegramUsernameKey, defaultTelegramBot)), "@"),
		},
		AI: AI{
			CloudURL:          strings.TrimRight(strings.TrimSpace(valueOrDefault(lookup, aiCloudURLKey, "")), "/"),
			CredentialsFile:   strings.TrimSpace(valueOrDefault(lookup, aiCredentialsKey, "")),
			Provider:          strings.ToLower(strings.TrimSpace(valueOrDefault(lookup, aiProviderKey, defaultAIProvider))),
			LlamaURL:          strings.TrimSpace(valueOrDefault(lookup, aiLlamaURLKey, defaultAILlamaURL)),
			ModelVersion:      strings.TrimSpace(valueOrDefault(lookup, aiModelVersionKey, defaultAIModel)),
			PollInterval:      defaultAIPoll,
			HeartbeatInterval: defaultAIHeartbeat,
			HTTPTimeout:       defaultAIHTTPTimeout,
			SignatureWindow:   defaultAISignatureWindow,
		},
	}
	if configuration.Integrations.CredentialKey, err = encryptionKeyValue(lookup, credentialKeyKey); err != nil {
		return Config{}, err
	}

	if configuration.HTTP.ShutdownTimeout, err = durationValue(lookup, shutdownTimeoutKey, defaultShutdown); err != nil {
		return Config{}, err
	}
	if configuration.Database.ConnectTimeout, err = durationValue(lookup, databaseTimeoutKey, defaultDatabaseWait); err != nil {
		return Config{}, err
	}
	if configuration.Database.MaxConnections, err = int32Value(lookup, databaseMaxConnsKey, defaultDatabaseMax); err != nil {
		return Config{}, err
	}
	if configuration.Database.MinConnections, err = int32Value(lookup, databaseMinConnsKey, defaultDatabaseMin); err != nil {
		return Config{}, err
	}
	if configuration.Auth.SessionTTL, err = durationValue(lookup, sessionTTLKey, defaultSessionTTL); err != nil {
		return Config{}, err
	}
	if configuration.AI.PollInterval, err = durationValue(lookup, aiPollIntervalKey, defaultAIPoll); err != nil {
		return Config{}, err
	}
	if configuration.AI.HeartbeatInterval, err = durationValue(lookup, aiHeartbeatKey, defaultAIHeartbeat); err != nil {
		return Config{}, err
	}
	if configuration.AI.HTTPTimeout, err = durationValue(lookup, aiHTTPTimeoutKey, defaultAIHTTPTimeout); err != nil {
		return Config{}, err
	}
	if configuration.AI.SignatureWindow, err = durationValue(lookup, aiSignatureWindowKey, defaultAISignatureWindow); err != nil {
		return Config{}, err
	}
	secureDefault := configuration.Environment == EnvironmentStaging || configuration.Environment == EnvironmentProduction
	if configuration.Auth.CookieSecure, err = boolValue(lookup, cookieSecureKey, secureDefault); err != nil {
		return Config{}, err
	}

	if configuration.Database.URL == "" && (configuration.Environment == EnvironmentDevelopment || configuration.Environment == EnvironmentTest) {
		configuration.Database.URL = defaultDatabaseURL
	}
	if err := configuration.Validate(); err != nil {
		return Config{}, err
	}

	return configuration, nil
}

// Validate rejects configuration that is unsafe or unknown to the runtime.
func (c Config) Validate() error {
	switch c.Environment {
	case EnvironmentDevelopment, EnvironmentTest, EnvironmentStaging, EnvironmentProduction:
	default:
		return fmt.Errorf("%s has unsupported value %q", environmentKey, c.Environment)
	}
	if strings.TrimSpace(c.HTTP.Address) == "" {
		return fmt.Errorf("%s must not be empty", httpAddressKey)
	}
	if c.HTTP.ShutdownTimeout <= 0 {
		return fmt.Errorf("%s must be positive", shutdownTimeoutKey)
	}
	if c.Database.ConnectTimeout <= 0 {
		return fmt.Errorf("%s must be positive", databaseTimeoutKey)
	}
	if c.Database.MaxConnections <= 0 || c.Database.MinConnections < 0 || c.Database.MinConnections > c.Database.MaxConnections {
		return fmt.Errorf("database connection limits are invalid")
	}
	if c.Auth.SessionTTL <= 0 {
		return fmt.Errorf("%s must be positive", sessionTTLKey)
	}
	if (c.Integrations.PublicBaseURL == "") != (len(c.Integrations.CredentialKey) == 0) {
		return fmt.Errorf("%s and %s must be configured together", publicBaseURLKey, credentialKeyKey)
	}
	if c.Integrations.PublicBaseURL != "" {
		parsed, err := url.Parse(c.Integrations.PublicBaseURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" ||
			len(c.Integrations.CredentialKey) != 32 {
			return fmt.Errorf("telegram integration configuration is invalid")
		}
	}
	if !telegramUsernamePattern.MatchString(c.Notifications.TelegramUsername) {
		return fmt.Errorf("%s has invalid format", telegramUsernameKey)
	}
	if c.Notifications.TelegramBotToken != "" && !telegramTokenPattern.MatchString(c.Notifications.TelegramBotToken) {
		return fmt.Errorf("%s has invalid format", telegramTokenKey)
	}
	if c.AI.Provider != "fake" && c.AI.Provider != "llama" {
		return fmt.Errorf("%s has unsupported value", aiProviderKey)
	}
	if c.AI.ModelVersion == "" || len(c.AI.ModelVersion) > 200 {
		return fmt.Errorf("%s has invalid value", aiModelVersionKey)
	}
	if c.AI.CloudURL != "" {
		parsed, err := url.Parse(c.AI.CloudURL)
		if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
			(parsed.Path != "" && parsed.Path != "/") ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") ||
			((c.Environment == EnvironmentStaging || c.Environment == EnvironmentProduction) && parsed.Scheme != "https") {
			return fmt.Errorf("%s has unsafe value", aiCloudURLKey)
		}
	}
	if c.AI.CredentialsFile != "" &&
		(c.Environment == EnvironmentStaging || c.Environment == EnvironmentProduction) &&
		!filepath.IsAbs(c.AI.CredentialsFile) {
		return fmt.Errorf("%s must be an absolute path", aiCredentialsKey)
	}
	if parsed, err := url.Parse(c.AI.LlamaURL); err != nil || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("%s has invalid value", aiLlamaURLKey)
	}
	if c.AI.PollInterval <= 0 || c.AI.HeartbeatInterval <= 0 || c.AI.HTTPTimeout <= 0 || c.AI.SignatureWindow <= 0 {
		return fmt.Errorf("AI runtime durations must be positive")
	}
	if (c.Environment == EnvironmentStaging || c.Environment == EnvironmentProduction) && !c.Auth.CookieSecure {
		return fmt.Errorf("%s must be true in %s", cookieSecureKey, c.Environment)
	}
	for _, origin := range c.HTTP.AllowedOrigins {
		parsed, err := url.Parse(origin)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("%s contains invalid origin %q", allowedOriginsKey, origin)
		}
	}
	return nil
}

var (
	telegramUsernamePattern = regexp.MustCompile(`^[A-Za-z0-9_]{5,32}$`)
	telegramTokenPattern    = regexp.MustCompile(`^[0-9]{5,20}:[A-Za-z0-9_-]{20,100}$`)
)

func valueOrDefault(lookup LookupEnv, key, fallback string) string {
	if value, ok := lookup(key); ok {
		return value
	}
	return fallback
}

func durationValue(lookup LookupEnv, key string, fallback time.Duration) (time.Duration, error) {
	raw, ok := lookup(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", key)
	}
	return value, nil
}

func int32Value(lookup LookupEnv, key string, fallback int32) (int32, error) {
	raw, ok := lookup(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return int32(value), nil
}

func boolValue(lookup LookupEnv, key string, fallback bool) (bool, error) {
	raw, ok := lookup(key)
	if !ok || raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return value, nil
}

func stringListValue(lookup LookupEnv, key string) []string {
	raw, ok := lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := make(map[string]struct{})
	values := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimRight(strings.TrimSpace(item), "/")
		if item == "" {
			continue
		}
		if _, exists := seen[item]; exists {
			continue
		}
		seen[item] = struct{}{}
		values = append(values, item)
	}
	return values
}

func encryptionKeyValue(lookup LookupEnv, key string) ([]byte, error) {
	raw, ok := lookup(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("%s must contain exactly 32 base64-encoded bytes", key)
	}
	return decoded, nil
}
