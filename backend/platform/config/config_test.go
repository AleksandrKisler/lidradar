package config

import (
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	configuration, err := Load(mapLookup(map[string]string{
		environmentKey: string(EnvironmentStaging),
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if configuration.Environment != EnvironmentStaging {
		t.Fatalf("Load().Environment = %q, want %q", configuration.Environment, EnvironmentStaging)
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

func TestLoadRejectsNilLookup(t *testing.T) {
	if _, err := Load(nil); err == nil {
		t.Fatal("Load() error = nil, want lookup error")
	}
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
