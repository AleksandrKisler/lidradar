package infrastructure

import (
	"context"
	"testing"
)

func TestStubTransportIsDeterministicAndLocal(t *testing.T) {
	transport := StubTransport{}
	first, retryable, err := transport.Send(context.Background(), "user", "Risk", "Reply now", "OPEN_RISK:risk-1")
	if err != nil || retryable || first == "" {
		t.Fatalf("Send() = (%q, %v, %v)", first, retryable, err)
	}
	second, _, _ := transport.Send(context.Background(), "user", "Risk", "Reply now", "OPEN_RISK:risk-1")
	if second != first {
		t.Fatalf("stub IDs differ: %q and %q", first, second)
	}
}
