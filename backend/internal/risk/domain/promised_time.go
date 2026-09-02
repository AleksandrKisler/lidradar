package domain

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PromisedDue — срок, извлечённый из текста обещания детерминированным
// разбором (LR-BE-1702). Разбор версионируется вместе с правилом R4: модель
// сообщает только факт обязательства, а срок вычисляется здесь и остаётся
// объяснимым через распознанный фрагмент.
type PromisedDue struct {
	At     time.Time
	Phrase string
}

// promisedDueHorizon ограничивает разумный срок обещания. Более далёкий или
// прошедший срок считается неопознанным и заменяется запасом в 60 рабочих
// минут (LR-BE-1703).
const promisedDueHorizon = 14 * 24 * time.Hour

var (
	promiseWordNumbers = map[string]int{
		"одну": 1, "один": 1, "одного": 1, "два": 2, "две": 2, "двух": 2, "три": 3, "трех": 3, "четыре": 4, "четырех": 4,
		"пять": 5, "пяти": 5, "шесть": 6, "шести": 6, "семь": 7, "семи": 7, "восемь": 8, "восьми": 8,
		"девять": 9, "девяти": 9, "десять": 10, "десяти": 10, "пятнадцать": 15, "пятнадцати": 15,
		"двадцать": 20, "двадцати": 20, "тридцать": 30, "тридцати": 30, "сорок": 40, "сорока": 40,
		"пятьдесят": 50, "пятидесяти": 50, "шестьдесят": 60, "шестидесяти": 60,
	}
	promiseWeekdays = map[string]time.Weekday{
		"понедельник": time.Monday, "вторник": time.Tuesday, "среду": time.Wednesday, "среда": time.Wednesday,
		"четверг": time.Thursday, "пятницу": time.Friday, "пятница": time.Friday,
		"субботу": time.Saturday, "суббота": time.Saturday, "воскресенье": time.Sunday,
	}
	promiseDayParts = map[string]int{
		"утром": 10, "до полудня": 12, "к полудню": 12, "до обеда": 12, "после обеда": 15,
		"вечером": 18, "до вечера": 18, "к вечеру": 18, "до конца дня": 18, "до конца рабочего дня": 18,
	}

	reAfter = regexp.MustCompile(
		`через (полчаса|полтора часа|час|(\d{1,3}|[а-я]+) ?(минут[ыу]?|мин\.?|час(?:а|ов)?))(?:[^а-я]|$)`)
	reWithin = regexp.MustCompile(
		`в течение (получаса|часа|рабочего дня|дня|(\d{1,3}|[а-я]+) ?(минут|час(?:а|ов)))(?:[^а-я]|$)`)
	reClock = regexp.MustCompile(
		`(?:(завтра|сегодня) )?(?:до|к|не позднее|не позже) (\d{1,2})(?:[:.](\d{2})| ?час(?:ов|а|ам))(?:[^а-я0-9:]|$)`)
	reWeekday = regexp.MustCompile(
		`(?:^|[^а-я])(?:в|во) (понедельник|вторник|среду|среда|четверг|пятницу|пятница|субботу|суббота|воскресенье)` +
			`(?: (утром|до полудня|к полудню|до обеда|после обеда|вечером|до вечера|к вечеру))?(?:[^а-я]|$)`)
	reTomorrow = regexp.MustCompile(
		`(?:^|[^а-я])(?:не позднее |не позже |до )?завтра` +
			`(?: (утром|до полудня|к полудню|до обеда|после обеда|вечером|до вечера|к вечеру))?(?:[^а-я]|$)`)
	reTodayPart = regexp.MustCompile(
		`(?:^|[^а-я])(?:сегодня )?(после обеда|до обеда|до полудня|к полудню|вечером|до вечера|к вечеру|до конца рабочего дня|до конца дня|утром)(?:[^а-я]|$)`)
	reToday = regexp.MustCompile(`(?:^|[^а-я])сегодня(?:[^а-я]|$)`)
)

// ParsePromisedDue разбирает явный срок в тексте обещания относительно момента
// отправки в часовом поясе точки. Возвращает false, когда срок не назван или
// назван неоднозначно: тогда действует запас в 60 рабочих минут.
func ParsePromisedDue(text string, from time.Time, loc *time.Location) (PromisedDue, bool) {
	if loc == nil || from.IsZero() {
		return PromisedDue{}, false
	}
	normalized := normalizePromise(text)
	if normalized == "" {
		return PromisedDue{}, false
	}
	local := from.In(loc)
	for _, parse := range []func(string, time.Time) (time.Time, string, bool){
		parseAfter, parseWithin, parseClock, parseWeekday, parseTomorrow, parseTodayPart, parseToday,
	} {
		due, phrase, ok := parse(normalized, local)
		if !ok {
			continue
		}
		if !due.After(from) || due.Sub(from) > promisedDueHorizon {
			return PromisedDue{}, false
		}
		return PromisedDue{At: due.UTC(), Phrase: strings.TrimSpace(phrase)}, true
	}
	return PromisedDue{}, false
}

func normalizePromise(text string) string {
	lowered := strings.ToLower(strings.ReplaceAll(text, "ё", "е"))
	return strings.Join(strings.Fields(lowered), " ")
}

func parseAfter(text string, from time.Time) (time.Time, string, bool) {
	match := reAfter.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, "", false
	}
	phrase := trimPhrase("через " + match[1])
	switch match[1] {
	case "полчаса":
		return from.Add(30 * time.Minute), phrase, true
	case "полтора часа":
		return from.Add(90 * time.Minute), phrase, true
	case "час":
		return from.Add(time.Hour), phrase, true
	}
	amount, ok := promiseNumber(match[2])
	if !ok {
		return time.Time{}, "", false
	}
	if strings.HasPrefix(match[3], "мин") {
		return from.Add(time.Duration(amount) * time.Minute), phrase, true
	}
	return from.Add(time.Duration(amount) * time.Hour), phrase, true
}

func parseWithin(text string, from time.Time) (time.Time, string, bool) {
	match := reWithin.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, "", false
	}
	phrase := trimPhrase("в течение " + match[1])
	switch match[1] {
	case "получаса":
		return from.Add(30 * time.Minute), phrase, true
	case "часа":
		return from.Add(time.Hour), phrase, true
	case "дня", "рабочего дня":
		return sameDay(from, 18, 0), phrase, true
	}
	amount, ok := promiseNumber(match[2])
	if !ok {
		return time.Time{}, "", false
	}
	if strings.HasPrefix(match[3], "мин") {
		return from.Add(time.Duration(amount) * time.Minute), phrase, true
	}
	return from.Add(time.Duration(amount) * time.Hour), phrase, true
}

func parseClock(text string, from time.Time) (time.Time, string, bool) {
	match := reClock.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, "", false
	}
	hour, err := strconv.Atoi(match[2])
	if err != nil || hour > 23 {
		return time.Time{}, "", false
	}
	minute := 0
	if match[3] != "" {
		if minute, err = strconv.Atoi(match[3]); err != nil || minute > 59 {
			return time.Time{}, "", false
		}
	} else if hour < 8 {
		// «до 5» без минут неоднозначно: утро или вечер.
		return time.Time{}, "", false
	}
	day := from
	if match[1] == "завтра" {
		day = from.AddDate(0, 0, 1)
	}
	return sameDay(day, hour, minute), trimPhrase(match[0]), true
}

func parseWeekday(text string, from time.Time) (time.Time, string, bool) {
	match := reWeekday.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, "", false
	}
	weekday, ok := promiseWeekdays[match[1]]
	if !ok {
		return time.Time{}, "", false
	}
	hour := 12
	if match[2] != "" {
		hour = promiseDayParts[match[2]]
	}
	days := (int(weekday) - int(from.Weekday()) + 7) % 7
	if days == 0 {
		days = 7
	}
	return sameDay(from.AddDate(0, 0, days), hour, 0), trimPhrase(match[0]), true
}

func parseTomorrow(text string, from time.Time) (time.Time, string, bool) {
	match := reTomorrow.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, "", false
	}
	hour := 12
	if match[1] != "" {
		hour = promiseDayParts[match[1]]
	}
	return sameDay(from.AddDate(0, 0, 1), hour, 0), trimPhrase(match[0]), true
}

func parseTodayPart(text string, from time.Time) (time.Time, string, bool) {
	match := reTodayPart.FindStringSubmatch(text)
	if match == nil {
		return time.Time{}, "", false
	}
	hour := promiseDayParts[match[1]]
	if match[1] == "утром" && from.Hour() >= 10 {
		// «Утром» после утра относится к другому дню и не разбирается.
		return time.Time{}, "", false
	}
	return sameDay(from, hour, 0), trimPhrase(match[0]), true
}

func parseToday(text string, from time.Time) (time.Time, string, bool) {
	if !reToday.MatchString(text) {
		return time.Time{}, "", false
	}
	return sameDay(from, 18, 0), "сегодня", true
}

func promiseNumber(value string) (int, bool) {
	if number, err := strconv.Atoi(value); err == nil {
		return number, number > 0
	}
	number, ok := promiseWordNumbers[value]
	return number, ok
}

// trimPhrase убирает захваченные регулярным выражением границы слова.
func trimPhrase(phrase string) string {
	return strings.Trim(phrase, " ,.;:!?—-()«»\"'")
}

func sameDay(day time.Time, hour, minute int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, day.Location())
}
