// Package config loads and validates LidRadar runtime configuration.
package config

import (
	"errors"
	"fmt"
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
	defaultHTTPAddress  = ":8080"
	defaultDatabaseURL  = "postgres://lidradar:lidradar@127.0.0.1:5432/lidradar?sslmode=disable"
	defaultShutdown     = 10 * time.Second
	defaultDatabaseWait = 5 * time.Second
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
	Environment Environment
	HTTP        HTTP
	Database    Database
}

// HTTP contains process-level HTTP server settings.
type HTTP struct {
	Address         string
	ShutdownTimeout time.Duration
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

	configuration := Config{
		Environment: Environment(rawEnvironment),
		HTTP: HTTP{
			Address:         valueOrDefault(lookup, httpAddressKey, defaultHTTPAddress),
			ShutdownTimeout: defaultShutdown,
		},
		Database: Database{
			URL:            valueOrDefault(lookup, databaseURLKey, ""),
			MaxConnections: defaultDatabaseMax,
			MinConnections: defaultDatabaseMin,
			ConnectTimeout: defaultDatabaseWait,
		},
	}

	var err error
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
