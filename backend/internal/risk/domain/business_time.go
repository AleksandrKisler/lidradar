package domain

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

var ErrInvalidBusinessHours = errors.New("invalid business hours")

// TimeOfDay is a wall-clock offset from local midnight.
type TimeOfDay time.Duration

func Clock(hour, minute int) TimeOfDay {
	return TimeOfDay(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

type BusinessPeriod struct {
	Open, Close TimeOfDay
}

// BusinessHours is a weekly schedule in an IANA timezone. Overnight periods
// are intentionally rejected; they must be represented as two weekday periods.
type BusinessHours struct {
	Timezone string
	Weekly   map[time.Weekday][]BusinessPeriod
}

func (h BusinessHours) validate() (*time.Location, error) {
	location, err := time.LoadLocation(h.Timezone)
	if err != nil {
		return nil, fmt.Errorf("%w: timezone: %v", ErrInvalidBusinessHours, err)
	}
	periodCount := 0
	for _, periods := range h.Weekly {
		periodCount += len(periods)
		ordered := append([]BusinessPeriod(nil), periods...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].Open < ordered[j].Open })
		for i, period := range ordered {
			if period.Open < 0 || period.Close > TimeOfDay(24*time.Hour) || period.Open >= period.Close ||
				(i > 0 && ordered[i-1].Close > period.Open) {
				return nil, ErrInvalidBusinessHours
			}
		}
	}
	if periodCount == 0 {
		return nil, ErrInvalidBusinessHours
	}
	return location, nil
}

func localDayPeriod(day time.Time, period BusinessPeriod, loc *time.Location) (time.Time, time.Time) {
	local := day.In(loc)
	midnight := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	return midnight.Add(time.Duration(period.Open)), midnight.Add(time.Duration(period.Close))
}

// AddBusinessTime carries elapsed time over closed periods and returns an instant.
func (h BusinessHours) AddBusinessTime(from time.Time, amount time.Duration) (time.Time, error) {
	loc, err := h.validate()
	if err != nil || from.IsZero() || amount < 0 {
		return time.Time{}, ErrInvalidBusinessHours
	}
	if amount == 0 {
		return from, nil
	}
	cursor := from
	// A valid weekly schedule guarantees discovery within seven days. The
	// larger guard supports long thresholds while protecting corrupt input.
	for days := 0; days < 3660; days++ {
		local := cursor.In(loc)
		periods := append([]BusinessPeriod(nil), h.Weekly[local.Weekday()]...)
		sort.Slice(periods, func(i, j int) bool { return periods[i].Open < periods[j].Open })
		for _, period := range periods {
			open, close := localDayPeriod(cursor, period, loc)
			if !cursor.Before(close) {
				continue
			}
			start := cursor
			if start.Before(open) {
				start = open
			}
			available := close.Sub(start)
			if amount <= available {
				return start.Add(amount), nil
			}
			amount -= available
		}
		next := local.AddDate(0, 0, 1)
		cursor = time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, loc)
	}
	return time.Time{}, ErrInvalidBusinessHours
}

// ElapsedBusinessTime measures only intersections with configured periods.
func (h BusinessHours) ElapsedBusinessTime(from, to time.Time) (time.Duration, error) {
	loc, err := h.validate()
	if err != nil || from.IsZero() || to.IsZero() || to.Before(from) {
		return 0, ErrInvalidBusinessHours
	}
	var elapsed time.Duration
	cursor := from
	for days := 0; !cursor.After(to) && days < 3660; days++ {
		local := cursor.In(loc)
		for _, period := range h.Weekly[local.Weekday()] {
			open, close := localDayPeriod(cursor, period, loc)
			start, end := open, close
			if start.Before(from) {
				start = from
			}
			if end.After(to) {
				end = to
			}
			if end.After(start) {
				elapsed += end.Sub(start)
			}
		}
		next := local.AddDate(0, 0, 1)
		cursor = time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, loc)
	}
	return elapsed, nil
}
