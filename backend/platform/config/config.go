// Package config loads and validates LidRadar runtime configuration.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	environmentKey      = "LIDRADAR_ENV"
	httpAddressKey      = "LIDRADAR_HTTP_ADDRESS"
	databaseURLKey      = "LIDRADAR_DATABASE_URL"
	shutdownTimeoutKey  = "LIDRADAR_SHUTDOWN_TIMEOUT"
	databaseMaxConnsKey = "LIDRADAR_DATABASE_MAX_CONNS"
	databaseMinConnsKey = "LIDRADAR_DATABASE_MIN_CONNS"
	databaseTimeoutKey  = "LIDRADAR_DATABASE_TIMEOUT"
	allowedOriginsKey   = "LIDRADAR_ALLOWED_ORIGINS"
	sessionTTLKey       = "LIDRADAR_SESSION_TTL"
	cookieSecureKey     = "LIDRADAR_COOKIE_SECURE"
	publicBaseURLKey    = "LIDRADAR_PUBLIC_BASE_URL"
	credentialKeyKey    = "LIDRADAR_INTEGRATION_ENCRYPTION_KEY"
	defaultHTTPAddress  = ":8080"
	defaultDatabaseURL  = "postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable"
	defaultShutdown     = 10 * time.Second
	defaultDatabaseWait = 5 * time.Second
	defaultSessionTTL   = 30 * 24 * time.Hour
	defaultDatabaseMax  = int32(10)
	defaultDatabaseMin  = int32(1)
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
	Environment  Environment
	HTTP         HTTP
	Database     Database
	Auth         Auth
	Integrations Integrations
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
