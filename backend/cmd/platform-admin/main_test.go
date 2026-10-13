package main

import (
	"bytes"
	"strings"
	"testing"
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
