package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	lifecycleapplication "lidradar/backend/internal/lifecycle/application"
	lifecycledomain "lidradar/backend/internal/lifecycle/domain"
)

func TestParseArgumentsAcceptsEveryCommandWithItsFlags(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want command
	}{
		"grant":               {[]string{"grant", "--email", " Admin@Example.com ", "--note", " первый "}, command{action: "grant", email: "Admin@Example.com", note: "первый", days: 14}},
		"revoke":              {[]string{"revoke", "--email", "a@example.com"}, command{action: "revoke", email: "a@example.com", days: 14}},
		"list":                {[]string{"list"}, command{action: "list", days: 14}},
		"invite по умолчанию": {[]string{"invite"}, command{action: "invite", days: 14}},
		"invite с флагами":    {[]string{"invite", "--note", "Салон Линия", "--days", "30"}, command{action: "invite", note: "Салон Линия", days: 30}},
		"invitations":         {[]string{"invitations"}, command{action: "invitations", days: 14}},
		"revoke-invitation":   {[]string{"revoke-invitation", "--id", "0199a1b2-0000-7000-8000-000000000001"}, command{action: "revoke-invitation", id: "0199a1b2-0000-7000-8000-000000000001", days: 14}},
		"allow-organization":  {[]string{"allow-organization", "--email", "owner@example.com"}, command{action: "allow-organization", email: "owner@example.com", days: 14}},
	} {
		got, err := parseArguments(tc.args, &bytes.Buffer{})
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v; want %+v", name, got, err, tc.want)
		}
	}
}

func TestParseArgumentsRejectsMissingOrInvalidInput(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"без действия":                 {nil, "использование"},
		"неизвестное действие":         {[]string{"delete"}, "неизвестное действие"},
		"grant без почты":              {[]string{"grant"}, "--email"},
		"allow-organization без почты": {[]string{"allow-organization"}, "--email"},
		"revoke-invitation без id":     {[]string{"revoke-invitation"}, "--id"},
		"срок ноль":                    {[]string{"invite", "--days", "0"}, "--days"},
		"срок больше предела":          {[]string{"invite", "--days", "61"}, "--days"},
		"лишний аргумент":              {[]string{"list", "extra"}, "использование"},
		"код в аргументах не нужен":    {[]string{"invite", "--code", "x"}, ""},
	} {
		_, err := parseArguments(tc.args, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want error containing %q", name, err, tc.want)
		}
	}
}

// erase-user (ADR 0059): необратимая команда требует основание и повторный ввод адреса, а сухой запуск — только их.

func TestParseArgumentsAcceptsEraseUserOnlyWithItsSafeguards(t *testing.T) {
	got, err := parseArguments([]string{"erase-user", "--email", "User@Example.com", "--note", "обращение №17", "--confirm-email", " user@example.com "}, &bytes.Buffer{})
	want := command{action: "erase-user", email: "User@Example.com", note: "обращение №17", confirmEmail: "user@example.com", days: 14}
	if err != nil || got != want {
		t.Errorf("боевой запуск: %+v, %v; want %+v", got, err, want)
	}
	got, err = parseArguments([]string{"erase-user", "--email", "user@example.com", "--note", "проверка", "--dry-run"}, &bytes.Buffer{})
	want = command{action: "erase-user", email: "user@example.com", note: "проверка", dryRun: true, days: 14}
	if err != nil || got != want {
		t.Errorf("сухой запуск без подтверждения: %+v, %v; want %+v", got, err, want)
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"без почты":             {[]string{"erase-user", "--note", "x", "--confirm-email", "a@example.com"}, "--email"},
		"без основания":         {[]string{"erase-user", "--email", "a@example.com", "--confirm-email", "a@example.com"}, "--note"},
		"основание из пробелов": {[]string{"erase-user", "--email", "a@example.com", "--note", "  ", "--confirm-email", "a@example.com"}, "--note"},
		"без подтверждения":     {[]string{"erase-user", "--email", "a@example.com", "--note", "x"}, "--confirm-email"},
		"другой адрес":          {[]string{"erase-user", "--email", "a@example.com", "--note", "x", "--confirm-email", "b@example.com"}, "--confirm-email"},
	} {
		if _, err := parseArguments(tc.args, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want error containing %q", name, err, tc.want)
		}
	}
	// Флаги erase-user не принимаются другими командами молча: они разбираются, но не меняют их смысл.
	if got, err := parseArguments([]string{"list", "--dry-run"}, &bytes.Buffer{}); err != nil || got.action != "list" {
		t.Errorf("list с флагом: %+v, %v", got, err)
	}
}

type fakeEraser struct {
	report lifecycleapplication.ErasureReport
	err    error
	email  string
	note   string
	dryRun bool
	calls  int
}

func (f *fakeEraser) EraseUser(_ context.Context, email, note string, dryRun bool) (lifecycleapplication.ErasureReport, error) {
	f.calls++
	f.email, f.note, f.dryRun = email, note, dryRun
	return f.report, f.err
}

func TestExecuteEraseReportsWhatWasDoneInStableOrder(t *testing.T) {
	eraser := &fakeEraser{report: lifecycleapplication.ErasureReport{
		UserID: "user-1", Outcome: lifecycledomain.ErasureDone,
		Counts: map[string]int64{"users": 1, "sessions": 2, "memberships": 1},
	}}
	var out bytes.Buffer
	parsed := command{action: "erase-user", email: "user@example.com", note: "обращение", confirmEmail: "user@example.com"}
	if err := executeErase(context.Background(), eraser, parsed, &out); err != nil {
		t.Fatalf("executeErase() error = %v", err)
	}
	if eraser.email != "user@example.com" || eraser.note != "обращение" || eraser.dryRun {
		t.Errorf("служба получила %q %q dryRun=%v", eraser.email, eraser.note, eraser.dryRun)
	}
	want := "пользователь обезличен: user-1. Затронуто строк:\n  memberships\t1\n  sessions\t2\n  users\t1\n"
	if out.String() != want {
		t.Errorf("вывод:\n%q\nожидался:\n%q", out.String(), want)
	}
}

func TestExecuteEraseDryRunSaysNothingWasChanged(t *testing.T) {
	eraser := &fakeEraser{report: lifecycleapplication.ErasureReport{
		UserID: "user-1", Outcome: lifecycledomain.ErasureDone, DryRun: true, Counts: map[string]int64{"users": 1},
	}}
	var out bytes.Buffer
	if err := executeErase(context.Background(), eraser, command{action: "erase-user", email: "u@example.com", note: "x", dryRun: true}, &out); err != nil {
		t.Fatal(err)
	}
	if !eraser.dryRun || !strings.Contains(out.String(), "сухой запуск") || !strings.Contains(out.String(), "ничего не изменено") ||
		strings.Contains(out.String(), "пользователь обезличен:") {
		t.Errorf("вывод сухого запуска: %q", out.String())
	}
}

func TestExecuteEraseExplainsBlockersAndFailsSoTheExitCodeIsNonZero(t *testing.T) {
	eraser := &fakeEraser{report: lifecycleapplication.ErasureReport{
		UserID: "user-1", Outcome: lifecycledomain.ErasureBlocked,
		Blockers: []lifecycledomain.Blocker{
			{Code: "SOLE_OWNER", OrganizationName: "Салон Лотос", HasOtherMembers: true},
			{Code: "SOLE_OWNER", OrganizationName: "Одноместная"},
			{Code: "PLATFORM_ADMIN"},
		},
	}}
	var out bytes.Buffer
	err := executeErase(context.Background(), eraser, command{action: "erase-user", email: "u@example.com", note: "x", confirmEmail: "u@example.com"}, &out)
	if err == nil || !strings.Contains(err.Error(), "ничего не изменено") {
		t.Fatalf("блокировка должна быть ошибкой: %v", err)
	}
	for _, want := range []string{"«Салон Лотос»", "передать владение", "«Одноместная»", "закрыть организацию", "администратор платформы", "revoke"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("вывод не содержит %q:\n%s", want, out.String())
		}
	}
}

func TestExecuteEraseTurnsSkippedAndUnknownIntoErrors(t *testing.T) {
	skipped := &fakeEraser{report: lifecycleapplication.ErasureReport{Outcome: lifecycledomain.ErasureSkipped}}
	if err := executeErase(context.Background(), skipped, command{email: "u@example.com", note: "x"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "уже обезличен") {
		t.Errorf("пропуск: %v", err)
	}
	unknown := &fakeEraser{err: lifecycleapplication.ErrUserNotFound}
	if err := executeErase(context.Background(), unknown, command{email: "u@example.com", note: "x"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "не найден") {
		t.Errorf("неизвестный адрес: %v", err)
	}
	broken := &fakeEraser{err: errors.New("database unavailable")}
	if err := executeErase(context.Background(), broken, command{email: "u@example.com", note: "x"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "database unavailable") {
		t.Errorf("сбой хранилища: %v", err)
	}
}
