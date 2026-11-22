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
// It returns "00:00" when now is before t.
func FormatElapsed(t, now time.Time) string {
	if now.Before(t) {
		return "00:00"
	}

	loc := now.Location()
	if loc == nil {
		loc = time.UTC
	}
	tLocal := t.In(loc)
	nowLocal := now.In(loc)

	// Round the elapsed duration to the nearest minute before splitting calendar
	// fields so 23h59m45s carries cleanly into the next day instead of "24:00".
	roundedDelta := nowLocal.Sub(tLocal).Round(time.Minute)
	if roundedDelta <= 0 {
		return "00:00"
	}
	end := tLocal.Add(roundedDelta).In(loc)

	y1, m1, d1 := tLocal.Date()
	y2, m2, d2 := end.Date()

	years := y2 - y1
	months := int(m2 - m1)
	days := d2 - d1

	totalMins := (end.Hour()*60 + end.Minute()) - (tLocal.Hour()*60 + tLocal.Minute())
	if totalMins < 0 {
		totalMins += 24 * 60
		days--
	}

	if days < 0 {
		months--
		prevYear, prevMonth := previousMonth(y2, m2)
		days += DaysInMonth(prevYear, prevMonth)
	}
	if months < 0 {
		years--
		months += 12
	}

	hours := totalMins / 60
	mins := totalMins % 60

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

func previousMonth(year int, month time.Month) (int, time.Month) {
	if month == time.January {
		return year - 1, time.December
	}
	return year, month - 1
}
