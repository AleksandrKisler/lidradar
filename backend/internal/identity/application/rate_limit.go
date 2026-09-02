package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

var ErrRateLimited = errors.New("authentication rate limited")

const (
	ScopeRegisterIP   = "REGISTER_IP"
	ScopeLoginIP      = "LOGIN_IP"
	ScopeLoginAccount = "LOGIN_ACCOUNT"
	ScopeRefreshIP    = "REFRESH_IP"

	registerIPLimit   = 5
	loginIPLimit      = 20
	loginAccountLimit = 5
	refreshIPLimit    = 60

	registerIPWindow   = time.Hour
	loginIPWindow      = time.Minute
	loginAccountWindow = 15 * time.Minute
	refreshIPWindow    = time.Minute
)

// RateLimitDecision — результат атомарного расходования одной попытки.
// ExpiresAt относится к текущему окну и позволяет вернуть честный Retry-After.
type RateLimitDecision struct {
	Allowed   bool
	ExpiresAt time.Time
}

// RateLimiter должен быть общим для всех экземпляров API. Рабочая композиция
// использует PostgreSQL; локальная память допустима только в тестах.
type RateLimiter interface {
	Take(context.Context, string, string, int, time.Duration, time.Time) (RateLimitDecision, error)
	Reset(context.Context, string, string) error
}

type RateLimitError struct{ RetryAfter time.Duration }

func (err RateLimitError) Error() string { return ErrRateLimited.Error() }
func (err RateLimitError) Unwrap() error { return ErrRateLimited }

func RateLimitRetryAfter(err error) time.Duration {
	var limited RateLimitError
	if errors.As(err, &limited) && limited.RetryAfter > 0 {
		return limited.RetryAfter
	}
	return time.Second
}

func rateSubject(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		value = "unknown"
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
