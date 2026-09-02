package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"lidradar/backend/internal/identity/domain"
)

type testPasswords struct{}

func (testPasswords) Hash(value string) (string, error) { return "hashed:" + value, nil }
func (testPasswords) Verify(value, hash string) (bool, error) {
	return hash == "hashed:"+value, nil
}

type testIDs struct{ next int }

func (i *testIDs) NewID() (string, error) {
	i.next++
	return fmt.Sprintf("id-%d", i.next), nil
}

type testTokens struct{ next int }

func (t *testTokens) NewToken() (string, string, error) {
	t.next++
	value := fmt.Sprintf("token-%d", t.next)
	return value, t.HashToken(value), nil
}

type testRateBucket struct {
	attempts  int
	expiresAt time.Time
}

type testRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]testRateBucket
}

func newTestRateLimiter() *testRateLimiter {
	return &testRateLimiter{buckets: map[string]testRateBucket{}}
}

func (limiter *testRateLimiter) Take(_ context.Context, scope, subject string, limit int, window time.Duration, now time.Time) (RateLimitDecision, error) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	key := scope + ":" + subject
	bucket := limiter.buckets[key]
	if !bucket.expiresAt.After(now) {
		bucket = testRateBucket{expiresAt: now.Add(window)}
	}
	bucket.attempts++
	limiter.buckets[key] = bucket
	return RateLimitDecision{Allowed: bucket.attempts <= limit, ExpiresAt: bucket.expiresAt}, nil
}

func (limiter *testRateLimiter) Reset(_ context.Context, scope, subject string) error {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	delete(limiter.buckets, scope+":"+subject)
	return nil
}
func (*testTokens) HashToken(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

type memoryRepository struct {
	users    map[string]domain.User
	sessions map[string]domain.Session
}

func newMemoryRepository() *memoryRepository {
	return &memoryRepository{users: map[string]domain.User{}, sessions: map[string]domain.Session{}}
}
func (r *memoryRepository) CreateUserSession(_ context.Context, user domain.User, session domain.Session) error {
	if _, exists := r.users[user.Email]; exists {
		return domain.ErrConflict
	}
	r.users[user.Email] = user
	r.sessions[session.TokenHash] = session
	return nil
}
func (r *memoryRepository) UserByEmail(_ context.Context, email string) (domain.User, bool, error) {
	user, ok := r.users[email]
	return user, ok, nil
}
func (r *memoryRepository) UserBySessionHash(_ context.Context, hash string, at time.Time) (domain.User, bool, error) {
	session, ok := r.sessions[hash]
	if !ok || session.RevokedAt != nil || !session.ExpiresAt.After(at) {
		return domain.User{}, false, nil
	}
	for _, user := range r.users {
		if user.ID == session.UserID {
			return user, true, nil
		}
	}
	return domain.User{}, false, nil
}
func (r *memoryRepository) CreateSession(_ context.Context, session domain.Session) error {
	r.sessions[session.TokenHash] = session
	return nil
}
func (r *memoryRepository) RotateSession(_ context.Context, oldHash string, replacement domain.Session, at time.Time) (domain.User, bool, error) {
	user, found, _ := r.UserBySessionHash(context.Background(), oldHash, at)
	if !found {
		return domain.User{}, false, nil
	}
	session := r.sessions[oldHash]
	revoked := at
	session.RevokedAt = &revoked
	r.sessions[oldHash] = session
	r.sessions[replacement.TokenHash] = replacement
	return user, true, nil
}
func (r *memoryRepository) RevokeSession(_ context.Context, hash string, at time.Time) error {
	if session, ok := r.sessions[hash]; ok {
		session.RevokedAt = &at
		r.sessions[hash] = session
	}
	return nil
}

func TestRegisterAuthenticateRefreshAndLogout(t *testing.T) {
	repository := newMemoryRepository()
	now := time.Date(2026, 8, 24, 10, 0, 0, 0, time.UTC)
	service := NewService(repository, newTestRateLimiter(), testPasswords{}, &testIDs{}, &testTokens{}, func() time.Time { return now }, 24*time.Hour)

	registered, err := service.Register(context.Background(), " OWNER@Example.com ", "very-secure-password", "Owner", Client{})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if registered.User.Email != "owner@example.com" || registered.Token == "" {
		t.Fatalf("Register() = %#v", registered)
	}
	for digest := range repository.sessions {
		if digest == registered.Token {
			t.Fatal("repository contains plaintext session token")
		}
	}
	if _, err := service.Authenticate(context.Background(), registered.Token); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}

	refreshed, err := service.Refresh(context.Background(), registered.Token, Client{})
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if refreshed.Token == registered.Token {
		t.Fatal("Refresh() did not rotate token")
	}
	if _, err := service.Authenticate(context.Background(), registered.Token); err != ErrUnauthenticated {
		t.Fatalf("old Authenticate() error = %v", err)
	}
	if err := service.Logout(context.Background(), refreshed.Token); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}
	if _, err := service.Authenticate(context.Background(), refreshed.Token); err != ErrUnauthenticated {
		t.Fatalf("logged out Authenticate() error = %v", err)
	}
}

func TestRegistrationConflictAndLoginErrorAreStable(t *testing.T) {
	repository := newMemoryRepository()
	service := NewService(repository, newTestRateLimiter(), testPasswords{}, &testIDs{}, &testTokens{}, time.Now, time.Hour)
	if _, err := service.Register(context.Background(), "owner@example.com", "very-secure-password", "Owner", Client{}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(context.Background(), "OWNER@example.com", "another-secure-password", "Other", Client{}); err != ErrEmailConflict {
		t.Fatalf("duplicate Register() error = %v", err)
	}
	if _, err := service.Login(context.Background(), "owner@example.com", "wrong", Client{}); err != ErrInvalidCredentials {
		t.Fatalf("Login() error = %v", err)
	}
}

func TestLoginBruteForceIsBlockedAndSuccessfulLoginResetsAccountCounter(t *testing.T) {
	repository := newMemoryRepository()
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	service := NewService(
		repository, newTestRateLimiter(), testPasswords{}, &testIDs{}, &testTokens{},
		func() time.Time { return now }, time.Hour,
	)
	client := Client{IPAddress: "192.0.2.10"}
	if _, err := service.Register(context.Background(), "owner@example.com", "very-secure-password", "Owner", client); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= loginAccountLimit; attempt++ {
		if _, err := service.Login(context.Background(), "OWNER@example.com", "wrong-password", client); err != ErrInvalidCredentials {
			t.Fatalf("attempt %d error = %v", attempt, err)
		}
	}
	if _, err := service.Login(context.Background(), "owner@example.com", "very-secure-password", client); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("blocked login error = %v", err)
	} else if retry := RateLimitRetryAfter(err); retry != loginAccountWindow {
		t.Fatalf("retry after = %s", retry)
	}

	now = now.Add(loginAccountWindow)
	if _, err := service.Login(context.Background(), "owner@example.com", "very-secure-password", client); err != nil {
		t.Fatalf("login after window = %v", err)
	}
	if _, err := service.Login(context.Background(), "owner@example.com", "wrong-password", client); err != ErrInvalidCredentials {
		t.Fatalf("counter was not reset after success: %v", err)
	}
}
