package application

import "strings"

// A completion/cancellation naming a different object cannot discharge this
// expectation just because both use the same verb (e.g. send a contract vs an
// invoice). Unknown wording remains model-assessed; this is a veto, not proof.
func agreementSubjectsCompatible(trigger, reply string) bool {
	left, right := agreementSubjects(trigger), agreementSubjects(reply)
	if len(left) == 0 || len(right) == 0 {
		return true
	}
	for _, subject := range left {
		found := false
		for _, other := range right {
			if other == subject {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func agreementSubjects(body string) []string {
	b := strings.ToLower(body)
	var result []string
	for _, group := range [][]string{
		{"invoice", "счёт", "счет", "invoice"}, {"contract", "договор", "contract"},
		{"estimate", "расчёт", "расчет", "estimate"}, {"payment", "оплат", "payment"},
		{"availability", "налич", "availability"}, {"schedule", "расписан", "schedule"},
		{"reschedule", "перенос", "перенес", "перенёс", "reschedul"},
		{"delivery", "достав", "delivery"}, {"link", "ссылк", "link"},
	} {
		if containsAny(b, group[1:]...) {
			result = append(result, group[0])
		}
	}
	return result
}

func cancelsAgreement(trigger, reply string) bool {
	return explicitCancellation(reply) && agreementSubjectsCompatible(trigger, reply)
}
