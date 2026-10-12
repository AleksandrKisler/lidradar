package domain

import (
	"strings"
	"testing"
	"time"
)

func TestLocationAndBusinessHoursValidation(t *testing.T) {
	now := time.Now()
	location, err := NewLocation("location", "tenant", "Main", "Europe/Moscow", 0, now)
	if err != nil || location.ResponseThresholdMinutes != 45 {
		t.Fatalf("NewLocation() = %#v, %v", location, err)
	}
	if _, err := NewLocation("location", "tenant", "Main", "Mars/Olympus", 45, now); err == nil {
		t.Fatal("NewLocation() accepted invalid timezone")
	}
	if _, err := NewBusinessHour("hour", "tenant", "location", 1, false, "21:00", "09:00"); err == nil {
		t.Fatal("NewBusinessHour() accepted reversed interval")
	}
}

// Договор и БД считают длину имени в символах (maxLength 200, char_length): кириллица в 150 символов — это 300 байт и допустимое имя.
func TestNamesAreLimitedByCharactersAndNotByBytes(t *testing.T) {
	now := time.Now()
	allowed, tooLong := strings.Repeat("я", 200), strings.Repeat("я", 201)
	if _, err := NewOrganization("org", allowed, "Europe/Moscow", "RUB", now); err != nil {
		t.Fatalf("an organization name of 200 characters (400 bytes) must be valid: %v", err)
	}
	if _, err := NewOrganization("org", tooLong, "Europe/Moscow", "RUB", now); err == nil {
		t.Fatal("an organization name of 201 characters must be rejected")
	}
	if _, err := NewLocation("location", "tenant", allowed, "Europe/Moscow", 45, now); err != nil {
		t.Fatalf("a location name of 200 characters must be valid: %v", err)
	}
	if _, err := NewLocation("location", "tenant", tooLong, "Europe/Moscow", 45, now); err == nil {
		t.Fatal("a location name of 201 characters must be rejected")
	}
}
