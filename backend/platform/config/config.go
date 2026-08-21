// Package config loads and validates LidRadar runtime configuration.
package config

import (
	"errors"
	"fmt"
)

const environmentKey = "LIDRADAR_ENV"

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

	configuration := Config{Environment: Environment(rawEnvironment)}
	if err := configuration.Validate(); err != nil {
		return Config{}, err
	}

	return configuration, nil
}

// Validate rejects configuration that is unsafe or unknown to the runtime.
func (c Config) Validate() error {
	switch c.Environment {
	case EnvironmentDevelopment, EnvironmentTest, EnvironmentStaging, EnvironmentProduction:
		return nil
	default:
		return fmt.Errorf("%s has unsupported value %q", environmentKey, c.Environment)
	}
}
