package domain

import (
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
