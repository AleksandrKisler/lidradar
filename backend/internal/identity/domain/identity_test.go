package domain

import (
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
