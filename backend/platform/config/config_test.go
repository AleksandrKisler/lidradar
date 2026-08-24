package config

import (
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
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if configuration.HTTP.Address != "127.0.0.1:9090" || configuration.HTTP.ShutdownTimeout != 3*time.Second {
		t.Fatalf("Load().HTTP = %#v", configuration.HTTP)
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
