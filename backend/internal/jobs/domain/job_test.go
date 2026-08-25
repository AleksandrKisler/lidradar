package domain

import (
	"errors"
	"testing"
	"time"
)

func TestRetryPolicyMatchesSpecification(t *testing.T) {
	want := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute}
	for index, duration := range want {
		if got := RetryDelay(index + 1); got != duration {
			t.Fatalf("RetryDelay(%d) = %s, нужно %s", index+1, got, duration)
		}
	}
	retryable, code := Classify(Permanent("INVALID_PAYLOAD", errors.New("неверные данные")))
	if retryable || code != "INVALID_PAYLOAD" {
		t.Fatalf("постоянная ошибка = retryable %v, code %q", retryable, code)
	}
	retryable, code = Classify(errors.New("неизвестная ошибка"))
	if !retryable || code != "UNCLASSIFIED_FAILURE" {
		t.Fatalf("неизвестная ошибка = retryable %v, code %q", retryable, code)
	}
}

func TestJobRejectsNonObjectPayload(t *testing.T) {
	now := time.Now()
	_, err := NewJob("job", "tenant", "test.v1", "dedup", []byte(`[]`), 0, now, now)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewJob() error = %v", err)
	}
}
