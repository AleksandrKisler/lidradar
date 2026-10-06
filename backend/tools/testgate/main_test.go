package main

import (
	"strings"
	"testing"
)

func TestGateRejectsGreenButIncompleteEvidence(t *testing.T) {
	for _, mode := range []string{"missing", "skip", "failed"} {
		t.Run(mode, func(t *testing.T) {
			r := report{Passed: append([]string{}, required...)}
			switch mode {
			case "missing":
				r.Passed = r.Passed[1:]
			case "skip":
				r.Skipped = []skipped{{Test: "some/new/database/test", Reason: "unavailable"}}
			case "failed":
				r.Failed = []string{"some/package"}
			}
			if validate(&r) == nil {
				t.Fatal("incomplete evidence accepted")
			}
		})
	}
}

func TestGateAllowsOnlyDocumentedSubprocessSkip(t *testing.T) {
	const input = `{"Action":"output","Package":"lidradar/backend/internal/jobs/infrastructure","Test":"TestJobClaimHelper","Output":"вспомогательный процесс запускается только из crash-test"}
{"Action":"skip","Package":"lidradar/backend/internal/jobs/infrastructure","Test":"TestJobClaimHelper"}
{"Action":"skip","Package":"new/package","Test":"TestDatabase"}`
	r := report{Passed: append([]string{}, required...)}
	if err := collect(strings.NewReader(input), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Skipped) != 2 || !r.Skipped[0].Expected || r.Skipped[1].Expected {
		t.Fatalf("skips: %+v", r.Skipped)
	}
	if validate(&r) == nil {
		t.Fatal("unexpected skip accepted")
	}
	r.Skipped = r.Skipped[:1]
	if err := validate(&r); err != nil {
		t.Fatal(err)
	}
	if err := collect(strings.NewReader("not JSON"), &r); err == nil {
		t.Fatal("broken stream accepted")
	}
}
