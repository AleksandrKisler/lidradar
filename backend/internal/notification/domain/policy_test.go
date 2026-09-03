package domain

import (
	"strings"
	"testing"
	"time"
)

var moscow = time.FixedZone("MSK", 3*3600)

func clock(t *testing.T, value string) ClockTime {
	t.Helper()
	parsed, err := ParseClockTime(value)
	if err != nil {
		t.Fatalf("время %q: %v", value, err)
	}
	return parsed
}

func local(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.ParseInLocation("2006-01-02 15:04", value, moscow)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func quietPreference(t *testing.T, riskType RiskType, mode DeliveryMode, start, end string) Preference {
	t.Helper()
	preference := DefaultPreference("tenant", "user", riskType)
	preference.DeliveryMode = mode
	preference.QuietHoursEnabled = true
	first, second := clock(t, start), clock(t, end)
	preference.QuietHoursStart, preference.QuietHoursEnd = &first, &second
	return preference
}

// ТЗ §46: R1/R3/R4 немедленно, R2/R5 сводкой; тихие часы 22:00–08:00 заполнены,
// но выключены; сводка в 09:00.
func TestDefaultPreferencesFollowSpecification(t *testing.T) {
	want := map[RiskType]DeliveryMode{
		RiskNoResponse: ModeImmediate, RiskBookingNotConfirmed: ModeImmediate, RiskPromiseNotFulfilled: ModeImmediate,
		RiskCustomerSilentAfterPrice: ModeDigest, RiskFollowUpCandidate: ModeDigest,
	}
	for _, riskType := range RiskTypes() {
		preference := DefaultPreference("tenant", "user", riskType)
		if err := preference.Validate(); err != nil || preference.Stored() || preference.DeliveryMode != want[riskType] ||
			preference.MinimumSeverity != SeverityLow || !preference.InAppEnabled || !preference.TelegramEnabled ||
			preference.QuietHoursEnabled || preference.QuietHoursStart.String() != "22:00" ||
			preference.QuietHoursEnd.String() != "08:00" || preference.DigestTime.String() != "09:00" {
			t.Fatalf("настройка по умолчанию %s = %#v, ошибка = %v", riskType, preference, err)
		}
	}
}

func TestPreferenceValidationRejectsDegenerateQuietHours(t *testing.T) {
	same := quietPreference(t, RiskNoResponse, ModeImmediate, "22:00", "22:00")
	if same.Validate() == nil {
		t.Fatal("совпадающие границы тихих часов приняты")
	}
	unbounded := DefaultPreference("tenant", "user", RiskNoResponse)
	unbounded.QuietHoursEnabled, unbounded.QuietHoursStart, unbounded.QuietHoursEnd = true, nil, nil
	if unbounded.Validate() == nil {
		t.Fatal("тихие часы без границ приняты")
	}
	half := DefaultPreference("tenant", "user", RiskNoResponse)
	half.QuietHoursEnd = nil
	if half.Validate() == nil {
		t.Fatal("одна граница тихих часов принята")
	}
	badSeverity := DefaultPreference("tenant", "user", RiskNoResponse)
	badSeverity.MinimumSeverity = "URGENT"
	badMode := DefaultPreference("tenant", "user", RiskNoResponse)
	badMode.DeliveryMode = "LATER"
	badType := DefaultPreference("tenant", "user", "UNKNOWN")
	for _, preference := range []Preference{badSeverity, badMode, badType} {
		if preference.Validate() == nil {
			t.Fatalf("некорректная настройка принята: %#v", preference)
		}
	}
	if _, err := ParseClockTime("24:00"); err == nil {
		t.Fatal("24:00 принято как время суток")
	}
	if _, err := ParseClockTime("9:00"); err == nil {
		t.Fatal("время без ведущего нуля принято")
	}
}

func TestDecideRespectsModeMinimumSeverityAndChannels(t *testing.T) {
	now := local(t, "2026-09-02 12:00")
	disabled := DefaultPreference("tenant", "user", RiskNoResponse)
	disabled.DeliveryMode = ModeDisabled
	if decision := disabled.Decide(SeverityCritical, now, moscow, true); decision.Deliver {
		t.Fatalf("выключенный режим доставил: %#v", decision)
	}
	highOnly := DefaultPreference("tenant", "user", RiskCustomerSilentAfterPrice)
	highOnly.MinimumSeverity = SeverityHigh
	if decision := highOnly.Decide(SeverityMedium, now, moscow, true); decision.Deliver {
		t.Fatalf("MEDIUM прошёл порог HIGH: %#v", decision)
	}
	if decision := highOnly.Decide(SeverityHigh, now, moscow, true); !decision.Deliver {
		t.Fatalf("HIGH не прошёл порог HIGH: %#v", decision)
	}
	noChannel := DefaultPreference("tenant", "user", RiskNoResponse)
	noChannel.InAppEnabled = false
	if decision := noChannel.Decide(SeverityHigh, now, moscow, false); decision.Deliver {
		t.Fatalf("без привязки и in-app доставлено: %#v", decision)
	}
	decision := noChannel.Decide(SeverityHigh, now, moscow, true)
	if !decision.Immediate() || decision.InApp || !decision.Telegram {
		t.Fatalf("только Telegram: %#v", decision)
	}
}

// LR-BE-RM-020: 22:00–08:00 через полночь; риск в 23:00 ждёт 08:00 следующего
// дня, риск в 12:00 доставляется немедленно.
func TestDecideQuietHoursWrapMidnight(t *testing.T) {
	preference := quietPreference(t, RiskNoResponse, ModeImmediate, "22:00", "08:00")
	cases := []struct {
		now, want string
	}{
		{"2026-09-02 23:00", "2026-09-03T08:00"},
		{"2026-09-02 22:00", "2026-09-03T08:00"},
		{"2026-09-03 07:30", "2026-09-03T08:00"},
		{"2026-09-02 12:00", ""},
		{"2026-09-03 08:00", ""},
	}
	for _, testCase := range cases {
		decision := preference.Decide(SeverityHigh, local(t, testCase.now), moscow, true)
		if !decision.Deliver || !decision.InApp || !decision.Telegram {
			t.Fatalf("%s: решение %#v", testCase.now, decision)
		}
		if testCase.want == "" {
			if !decision.Immediate() {
				t.Fatalf("%s: ожидалась немедленная доставка, получено %#v", testCase.now, decision)
			}
			continue
		}
		wantAt, _ := time.ParseInLocation(SlotLayout, testCase.want, moscow)
		if decision.Immediate() || decision.Slot != testCase.want || !decision.DeliverAt.Equal(wantAt) ||
			decision.Reason != DeferQuietHours || decision.DeliverAt.Location() != time.UTC {
			t.Fatalf("%s: ожидался слот %s, получено %#v", testCase.now, testCase.want, decision)
		}
	}
	day := quietPreference(t, RiskNoResponse, ModeImmediate, "13:00", "14:00")
	if decision := day.Decide(SeverityHigh, local(t, "2026-09-02 13:30"), moscow, true); decision.Slot != "2026-09-02T14:00" {
		t.Fatalf("дневные тихие часы: %#v", decision)
	}
	if decision := day.Decide(SeverityHigh, local(t, "2026-09-02 12:59"), moscow, true); !decision.Immediate() {
		t.Fatalf("до дневных тихих часов: %#v", decision)
	}
}

func TestDecideDigestUsesNextOccurrenceAndQuietHours(t *testing.T) {
	preference := DefaultPreference("tenant", "user", RiskFollowUpCandidate)
	if decision := preference.Decide(SeverityMedium, local(t, "2026-09-02 08:00"), moscow, false); decision.Slot != "2026-09-02T09:00" ||
		decision.Reason != DeferDigest || !decision.InApp || decision.Telegram {
		t.Fatalf("сводка до 09:00: %#v", decision)
	}
	if decision := preference.Decide(SeverityMedium, local(t, "2026-09-02 09:00"), moscow, false); decision.Slot != "2026-09-03T09:00" {
		t.Fatalf("сводка ровно в 09:00: %#v", decision)
	}
	night := quietPreference(t, RiskFollowUpCandidate, ModeDigest, "22:00", "08:00")
	night.DigestTime = clock(t, "23:00")
	if decision := night.Decide(SeverityMedium, local(t, "2026-09-02 12:00"), moscow, false); decision.Slot != "2026-09-03T08:00" {
		t.Fatalf("сводка внутри тихих часов не сдвинута: %#v", decision)
	}
}

func TestComposeDigestLimitsLinesAndLabelsRisks(t *testing.T) {
	entries := make([]DigestEntry, 0, 17)
	for index := 0; index < 17; index++ {
		entries = append(entries, DigestEntry{
			Item:     DigestItem{RiskType: RiskCustomerSilentAfterPrice, Reason: DeferDigest},
			Severity: SeverityMedium, Status: "OPEN", Contact: "Клиент", DetectedAt: local(t, "2026-09-02 12:00"),
		})
	}
	entries[0].Item.RiskType, entries[0].Severity, entries[0].Contact = RiskNoResponse, SeverityHigh, strings.Repeat("И", 60)
	title, body := ComposeDigest(entries, moscow)
	if title != "Сводка рисков: 17" || len(body) > 2000 || !strings.Contains(body, "15. ") || strings.Contains(body, "16. ") ||
		!strings.Contains(body, "…и ещё 2.") || !strings.Contains(body, "1. Высокий · клиент ждёт ответа: "+strings.Repeat("И", 40)+"…, с 02.09 12:00") ||
		!strings.HasSuffix(body, "Откройте Radar для подробностей.") {
		t.Fatalf("сводка = %q\n%s", title, body)
	}
	quiet := []DigestEntry{{
		Item: DigestItem{RiskType: RiskBookingNotConfirmed, Reason: DeferQuietHours}, Severity: SeverityCritical, Status: "OPEN",
		DetectedAt: local(t, "2026-09-02 23:05"),
	}}
	title, body = ComposeDigest(quiet, moscow)
	if title != "Риски за тихие часы: 1" || !strings.Contains(body, "1. Критический · запись не подтверждена: клиент, с 02.09 23:05") {
		t.Fatalf("сводка тихих часов = %q\n%s", title, body)
	}
}

func TestNotificationKindsAndDigestItems(t *testing.T) {
	at := local(t, "2026-09-02 12:00")
	opened, err := NewNotification("n1", "tenant", "user", "risk", "Заголовок", "Текст", at)
	if err != nil || opened.Kind != KindRiskOpened || opened.DedupKey != "risk:risk:opened:user:user" || opened.Slot() != "" {
		t.Fatalf("уведомление об открытии = %#v, %v", opened, err)
	}
	escalated, err := NewEscalation("n2", "tenant", "owner", "risk", "Эскалация", "Текст", at)
	if err != nil || escalated.DedupKey != "risk:risk:escalated:user:owner" {
		t.Fatalf("эскалация = %#v, %v", escalated, err)
	}
	digest, err := NewDigest("n3", "tenant", "user", "2026-09-03T08:00", "Сводка", "Текст", at)
	if err != nil || digest.RiskID != "" || digest.Slot() != "2026-09-03T08:00" || digest.DedupKey != "digest:user:user:2026-09-03T08:00" {
		t.Fatalf("сводка = %#v, %v", digest, err)
	}
	if _, err := NewDigest("n4", "tenant", "user", "tomorrow", "Сводка", "Текст", at); err == nil {
		t.Fatal("некорректный слот принят")
	}
	delivery, err := NewDelivery("d1", digest, "user", ChannelInApp, at)
	if err != nil || delivery.Kind != KindRiskDigest || delivery.Actions() {
		t.Fatalf("доставка сводки = %#v, %v", delivery, err)
	}
	if opened, _ := NewDelivery("d2", opened, "7001", ChannelTelegram, at); !opened.Actions() {
		t.Fatal("доставка риска лишилась кнопок")
	}
	preference := DefaultPreference("tenant", "user", RiskFollowUpCandidate)
	deferred := preference.Decide(SeverityMedium, at, moscow, false)
	item, err := NewDigestItem("i1", "tenant", "user", "risk", RiskFollowUpCandidate, deferred, at)
	if err != nil || item.Slot != "2026-09-03T09:00" || item.Reason != DeferDigest || !item.InApp || item.Telegram {
		t.Fatalf("элемент сводки = %#v, %v", item, err)
	}
	immediate := DefaultPreference("tenant", "user", RiskNoResponse).Decide(SeverityHigh, at, moscow, true)
	if _, err := NewDigestItem("i2", "tenant", "user", "risk", RiskNoResponse, immediate, at); err == nil {
		t.Fatal("немедленное решение стало элементом сводки")
	}
	policy := EscalationPolicy{Enabled: true, After: 30 * time.Minute}
	if !policy.Applies(SeverityHigh) || policy.Applies(SeverityMedium) || (EscalationPolicy{}).Applies(SeverityCritical) {
		t.Fatal("политика эскалации применяется неверно")
	}
}
