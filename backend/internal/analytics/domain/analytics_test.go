package domain

import (
	"testing"
	"time"
)

var moscow = time.FixedZone("MSK", 3*3600)

// LR-BE-2202: границы окна — календарные даты в часовом поясе организации;
// по умолчанию 30 дней, включая сегодняшний день по этому поясу.
func TestResolvePeriodUsesOrganizationTimezoneAndDefaults(t *testing.T) {
	now := time.Date(2026, 9, 2, 22, 30, 0, 0, time.UTC) // 2026-09-03 01:30 по Москве
	period, err := ResolvePeriod("", "", now, moscow)
	if err != nil || period.FromDate != "2026-08-05" || period.ToDate != "2026-09-03" || period.Timezone != "MSK" ||
		!period.From.Equal(time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)) || !period.To.Equal(time.Date(2026, 9, 3, 21, 0, 0, 0, time.UTC)) {
		t.Fatalf("период по умолчанию = %#v, %v", period, err)
	}
	explicit, err := ResolvePeriod("2026-08-01", "2026-08-31", now, moscow)
	if err != nil || !explicit.From.Equal(time.Date(2026, 7, 31, 21, 0, 0, 0, time.UTC)) ||
		!explicit.To.Equal(time.Date(2026, 8, 31, 21, 0, 0, 0, time.UTC)) ||
		!explicit.Contains(time.Date(2026, 8, 31, 20, 59, 0, 0, time.UTC)) || explicit.Contains(time.Date(2026, 8, 31, 21, 0, 0, 0, time.UTC)) ||
		explicit.Contains(time.Date(2026, 7, 31, 20, 59, 0, 0, time.UTC)) {
		t.Fatalf("явный период = %#v, %v", explicit, err)
	}
	if day, err := ResolvePeriod("2026-08-10", "2026-08-10", now, moscow); err != nil || day.To.Sub(day.From) != 24*time.Hour {
		t.Fatalf("однодневное окно = %#v, %v", day, err)
	}
	if long, err := ResolvePeriod("2025-09-02", "2026-09-02", now, moscow); err != nil || long.FromDate != "2025-09-02" {
		t.Fatalf("окно в 366 дней отклонено: %#v, %v", long, err)
	}
	for name, dates := range map[string][2]string{
		"обратный порядок":  {"2026-09-10", "2026-09-01"},
		"без ведущего нуля": {"2026-8-1", "2026-08-31"},
		"длиннее года":      {"2025-09-01", "2026-09-02"},
		"мусор":             {"вчера", ""},
	} {
		if _, err := ResolvePeriod(dates[0], dates[1], now, moscow); err == nil {
			t.Fatalf("%s: некорректный период принят", name)
		}
	}
	if _, err := ResolvePeriod("", "", now, nil); err == nil {
		t.Fatal("период без часового пояса принят")
	}
	if _, err := ResolvePeriod("", "", time.Time{}, moscow); err == nil {
		t.Fatal("период без текущего времени принят")
	}
}

func TestRisksFromTypesFillsCanonicalOrderAndTotals(t *testing.T) {
	risks := RisksFromTypes([]RiskTypeMetrics{
		{RiskType: "FOLLOW_UP_CANDIDATE", RiskCounters: RiskCounters{Detected: 2, Resolved: 1}},
		{RiskType: "NO_RESPONSE", RiskCounters: RiskCounters{Detected: 3, Acted: 2, FalsePositive: 1}},
	})
	if len(risks.ByType) != 5 || risks.ByType[0].RiskType != "NO_RESPONSE" || risks.ByType[4].RiskType != "FOLLOW_UP_CANDIDATE" ||
		risks.ByType[1].Detected != 0 || risks.Detected != 5 || risks.Acted != 2 || risks.Resolved != 1 || risks.FalsePositive != 1 {
		t.Fatalf("разрез по типам = %#v", risks)
	}
	if empty := RisksFromTypes(nil); len(empty.ByType) != 5 || empty.Detected != 0 {
		t.Fatalf("пустой разрез = %#v", empty)
	}
}
