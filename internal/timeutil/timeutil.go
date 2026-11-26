package timeutil

import (
	"fmt"
	"strings"
	"time"
)

// IsPriorDay reports whether t falls on a calendar date before now in loc.
// If loc is nil, IsPriorDay uses time.UTC.
func IsPriorDay(t, now time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.UTC
	}
	nowInLoc := now.In(loc)
	tInLoc := t.In(loc)
	todayMidnight := time.Date(nowInLoc.Year(), nowInLoc.Month(), nowInLoc.Day(), 0, 0, 0, 0, loc)
	tDate := time.Date(tInLoc.Year(), tInLoc.Month(), tInLoc.Day(), 0, 0, 0, 0, loc)
	return tDate.Before(todayMidnight)
}

// DaysInMonth returns the number of calendar days in the given year and month.
func DaysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// FormatElapsed formats the elapsed calendar duration from t to now as
// "[Yy ][Mm ][Dd ]HH:MM", omitting leading zero calendar units.
// It computes calendar steps in UTC so daylight saving shifts never distort elapsed time.
// It returns "00:00" when now is before t.
func FormatElapsed(t, now time.Time) string {
	if now.Before(t) {
		return "00:00"
	}

	// Round the elapsed duration to the nearest minute before splitting calendar
	// fields so 23h59m45s carries cleanly into the next day instead of "24:00".
	roundedDelta := now.Sub(t).Round(time.Minute)
	if roundedDelta <= 0 {
		return "00:00"
	}

	startUTC := t.UTC()
	endUTC := startUTC.Add(roundedDelta)

	y1, m1, _ := startUTC.Date()
	y2, m2, _ := endUTC.Date()
	totalMonths := (y2-y1)*12 + int(m2-m1)
	if totalMonths < 0 {
		totalMonths = 0
	}

	cursor := addCalendarMonthsUTC(startUTC, totalMonths)
	for totalMonths > 0 && cursor.After(endUTC) {
		totalMonths--
		cursor = addCalendarMonthsUTC(startUTC, totalMonths)
	}

	years := totalMonths / 12
	months := totalMonths % 12

	rem := endUTC.Sub(cursor)
	if rem < 0 {
		rem = 0
	}
	days := int(rem / (24 * time.Hour))
	rem %= 24 * time.Hour
	hours := int(rem / time.Hour)
	rem %= time.Hour
	mins := int(rem / time.Minute)

	var sb strings.Builder
	if years > 0 {
		fmt.Fprintf(&sb, "%dy ", years)
	}
	if months > 0 || years > 0 {
		fmt.Fprintf(&sb, "%dm ", months)
	}
	if days > 0 || months > 0 || years > 0 {
		fmt.Fprintf(&sb, "%dd ", days)
	}
	fmt.Fprintf(&sb, "%02d:%02d", hours, mins)

	return sb.String()
}

// addCalendarMonthsUTC advances t by months, clamping day-of-month to the target month's length.
func addCalendarMonthsUTC(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	total := (y*12 + int(m) - 1) + months
	targetYear := total / 12
	targetMonth := time.Month(total%12 + 1)
	maxDay := DaysInMonth(targetYear, targetMonth)
	if d > maxDay {
		d = maxDay
	}
	return time.Date(targetYear, targetMonth, d, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
}
