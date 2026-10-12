package domain

import (
	"strings"
	"testing"
	"time"
)

func TestNewUserCanonicalizesEmail(t *testing.T) {
	user, err := NewUser("id", " Owner@Example.COM ", "hash", " Owner ", time.Now())
	if err != nil {
		t.Fatalf("NewUser() error = %v", err)
	}
	if user.Email != "owner@example.com" || user.DisplayName != "Owner" {
		t.Fatalf("user = %#v", user)
	}
}

func TestNewSessionRejectsPlainOrShortHash(t *testing.T) {
	now := time.Now()
	if _, err := NewSession("id", "user", "plaintext", now, now.Add(time.Hour), "", ""); err == nil {
		t.Fatal("NewSession() accepted a non-SHA-256 token hash")
	}
}

func TestDisplayNameIsLimitedByCharactersAndNotByBytes(t *testing.T) {
	if _, err := NewUser("id", "owner@example.com", "hash", strings.Repeat("ы", 200), time.Now()); err != nil {
		t.Fatalf("a display name of 200 characters (400 bytes) must be valid: %v", err)
	}
	if _, err := NewUser("id", "owner@example.com", "hash", strings.Repeat("ы", 201), time.Now()); err == nil {
		t.Fatal("a display name of 201 characters must be rejected")
	}
}
