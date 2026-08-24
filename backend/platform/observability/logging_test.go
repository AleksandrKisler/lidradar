package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
)

func TestNewLoggerWritesRequiredProcessFields(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(&output, "lidradar-api", "test")
	logger.Info("started", "event", "runtime.started")

	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("log output is not JSON: %v", err)
	}
	if record["service"] != "lidradar-api" || record["environment"] != "test" || record["event"] != "runtime.started" {
		t.Fatalf("log record = %#v", record)
	}
}

func TestContextLoggerRoundTrip(t *testing.T) {
	logger := NewLogger(&bytes.Buffer{}, "test", "test")
	ctx := WithLogger(context.Background(), logger)
	if Logger(ctx) != logger {
		t.Fatal("Logger() did not return the contextual logger")
	}
}
