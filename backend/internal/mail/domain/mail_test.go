package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var created = time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)

func TestNewMessageNormalizesAndValidates(t *testing.T) {
	message, err := NewMessage("id-1", KindPasswordReset, "  Owner@Example.COM ", "  Сброс пароля  ", "Строка 1\nСтрока 2\r\n\tОтступ", created, created.Add(time.Hour))
	if err != nil {
		t.Fatalf("NewMessage() error = %v", err)
	}
	if message.To != "owner@example.com" || message.Subject != "Сброс пароля" || message.RecipientDomain() != "example.com" {
		t.Fatalf("message = %#v", message)
	}
	if !message.ExpiresAt.Equal(created.Add(time.Hour)) || message.CreatedAt.Location() != time.UTC {
		t.Fatalf("times must be UTC: %#v", message)
	}
}

func TestNewMessageRejectsUnsafeOrMalformedInput(t *testing.T) {
	valid := func(mutate func(*[7]string)) error {
		fields := [7]string{"id-1", "PASSWORD_RESET", "user@example.com", "Тема", "Текст", "", ""}
		mutate(&fields)
		_, err := NewMessage(fields[0], Kind(fields[1]), fields[2], fields[3], fields[4], created, created.Add(time.Hour))
		return err
	}
	cases := map[string]func(*[7]string){
		"пустой идентификатор":     func(f *[7]string) { f[0] = "" },
		"вид со строчными":         func(f *[7]string) { f[1] = "password_reset" },
		"пустой вид":               func(f *[7]string) { f[1] = "" },
		"слишком длинный вид":      func(f *[7]string) { f[1] = "A" + strings.Repeat("B", 50) },
		"адрес с именем":           func(f *[7]string) { f[2] = "Owner <owner@example.com>" },
		"адрес без домена":         func(f *[7]string) { f[2] = "owner" },
		"пустой адрес":             func(f *[7]string) { f[2] = "" },
		"два адреса":               func(f *[7]string) { f[2] = "a@example.com, b@example.com" },
		"внедрение заголовка":      func(f *[7]string) { f[2] = "a@example.com\r\nBcc: victim@example.com" },
		"тема с переводом строки":  func(f *[7]string) { f[3] = "Тема\r\nBcc: victim@example.com" },
		"тема с управляющим":       func(f *[7]string) { f[3] = "Тема\x07" },
		"пустая тема":              func(f *[7]string) { f[3] = "   " },
		"слишком длинная тема":     func(f *[7]string) { f[3] = strings.Repeat("я", maxSubjectRunes+1) },
		"пустой текст":             func(f *[7]string) { f[4] = "" },
		"текст с NUL":              func(f *[7]string) { f[4] = "a\x00b" },
		"текст с управляющим":      func(f *[7]string) { f[4] = "a\x1bb" },
		"недопустимый UTF-8":       func(f *[7]string) { f[4] = "a\xffb" },
		"слишком длинный текст":    func(f *[7]string) { f[4] = strings.Repeat("a", maxTextBytes+1) },
		"слишком длинный адрес":    func(f *[7]string) { f[2] = strings.Repeat("a", 250) + "@example.com" },
		"текст из одних переносов": func(f *[7]string) { f[4] = "" },
	}
	for name, mutate := range cases {
		if err := valid(mutate); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error = %v, want ErrInvalid", name, err)
		}
	}
	if err := valid(func(*[7]string) {}); err != nil {
		t.Fatalf("the baseline must be valid: %v", err)
	}
	if _, err := NewMessage("id", KindPasswordReset, "a@example.com", "Тема", "Текст", created, created); !errors.Is(err, ErrInvalid) {
		t.Errorf("a message that expires at creation must be rejected, error = %v", err)
	}
	if _, err := NewMessage("id", KindPasswordReset, "a@example.com", "Тема", "Текст", time.Time{}, created); !errors.Is(err, ErrInvalid) {
		t.Errorf("a message without a creation time must be rejected, error = %v", err)
	}
}

func TestRetryDelayGrowsAndSaturates(t *testing.T) {
	want := []time.Duration{5 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for attempt, expected := range want {
		if got := RetryDelay(attempt + 1); got != expected {
			t.Errorf("RetryDelay(%d) = %s, want %s", attempt+1, got, expected)
		}
	}
	if RetryDelay(0) != 10*time.Minute || RetryDelay(-3) != 10*time.Minute {
		t.Error("out-of-range attempts must use the longest pause, not a zero one")
	}
}

func TestSendFailureUnwrapsAndNamesItsCode(t *testing.T) {
	cause := errors.New("connection reset")
	failure := &SendFailure{Code: CodeSMTPNetwork, Retryable: true, Err: cause}
	if !errors.Is(failure, cause) || !strings.Contains(failure.Error(), CodeSMTPNetwork) || !strings.Contains(failure.Error(), "connection reset") {
		t.Fatalf("failure = %v", failure)
	}
	if (&SendFailure{Code: CodeExpired}).Error() != CodeExpired {
		t.Fatal("a failure without a cause reports only its code")
	}
}
