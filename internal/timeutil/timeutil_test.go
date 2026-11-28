package timeutil

import (
	"testing"
	"time"
)

func TestIsPriorDay(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}

	nowZurich := time.Date(2026, 6, 15, 0, 30, 0, 0, zurich)

	tests := []struct {
		name string
		t    time.Time
		now  time.Time
		loc  *time.Location
		want bool
	}{
		{
			name: "yesterday in same tz",
			t:    time.Date(2026, 6, 14, 23, 59, 59, 0, zurich),
			now:  nowZurich,
			loc:  zurich,
			want: true,
		},
		{
			name: "today midnight in same tz",
			t:    time.Date(2026, 6, 15, 0, 0, 0, 0, zurich),
			now:  nowZurich,
			loc:  zurich,
			want: false,
		},
		{
			name: "tomorrow in same tz",
			t:    time.Date(2026, 6, 16, 0, 0, 0, 0, zurich),
			now:  nowZurich,
			loc:  zurich,
			want: false,
		},
		{
			name: "UTC instant that is already today in Zurich",
			t:    time.Date(2026, 6, 14, 22, 15, 0, 0, time.UTC), // 2026-06-15 00:15 CEST
			now:  nowZurich,
			loc:  zurich,
			want: false,
		},
		{
			name: "UTC instant that is still yesterday in Zurich",
			t:    time.Date(2026, 6, 14, 21, 59, 0, 0, time.UTC), // 2026-06-14 23:59 CEST
			now:  nowZurich,
			loc:  zurich,
			want: true,
		},
		{
			name: "New York time that is today in Zurich",
			t:    time.Date(2026, 6, 14, 18, 10, 0, 0, ny), // 2026-06-15 00:10 CEST
			now:  nowZurich,
			loc:  zurich,
			want: false,
		},
		{
			name: "nil location falls back to UTC",
			t:    time.Date(2026, 6, 14, 23, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 6, 15, 0, 1, 0, 0, time.UTC),
			loc:  nil,
			want: true,
		},
		{
			name: "DST spring forward boundary",
			t:    time.Date(2026, 3, 29, 0, 30, 0, 0, zurich),
			now:  time.Date(2026, 3, 29, 3, 30, 0, 0, zurich),
			loc:  zurich,
			want: false,
		},
		{
			name: "DST fall-back repeated hour same calendar day",
			t:    time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC), // 02:30 CEST (+0200)
			now:  time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC), // 02:30 CET (+0100)
			loc:  zurich,
			want: false,
		},
		{
			name: "DST fall-back repeated hour after previous day",
			t:    time.Date(2026, 10, 24, 21, 59, 0, 0, time.UTC), // 2026-10-24 23:59 CEST
			now:  time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC),  // 2026-10-25 02:30 CET
			loc:  zurich,
			want: true,
		},
		{
			name: "prior year returns true",
			t:    time.Date(2025, 12, 31, 23, 59, 0, 0, zurich),
			now:  time.Date(2026, 1, 1, 0, 1, 0, 0, zurich),
			loc:  zurich,
			want: true,
		},
		{
			name: "prior month returns true",
			t:    time.Date(2026, 5, 31, 23, 59, 0, 0, zurich),
			now:  time.Date(2026, 6, 1, 0, 1, 0, 0, zurich),
			loc:  zurich,
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IsPriorDay(tc.t, tc.now, tc.loc)
			if got != tc.want {
				t.Errorf("IsPriorDay(%v, %v, %v) = %v, want %v", tc.t, tc.now, tc.loc, got, tc.want)
			}
		})
	}
}

func TestDaysInMonth(t *testing.T) {
	tests := []struct {
		year  int
		month time.Month
		want  int
	}{
		{2024, time.February, 29}, // leap year
		{2025, time.February, 28}, // common year
		{2000, time.February, 29}, // century leap year
		{1900, time.February, 28}, // century non-leap year
		{2026, time.January, 31},
		{2026, time.April, 30},
		{2026, time.December, 31},
	}

	for _, tc := range tests {
		got := DaysInMonth(tc.year, tc.month)
		if got != tc.want {
			t.Errorf("DaysInMonth(%d, %s) = %d, want %d", tc.year, tc.month, got, tc.want)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	zurich, err := time.LoadLocation("Europe/Zurich")
	if err != nil {
		t.Fatalf("load Europe/Zurich: %v", err)
	}

	tests := []struct {
		name string
		t    time.Time
		now  time.Time
		want string
	}{
		{
			name: "future timestamp returns 00:00",
			t:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 6, 15, 11, 59, 0, 0, time.UTC),
			want: "00:00",
		},
		{
			name: "equal timestamps return 00:00",
			t:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
			want: "00:00",
		},
		{
			name: "23h59m45s rounds to 1d 00:00 not 24:00",
			t:    time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 6, 16, 9, 59, 45, 0, time.UTC),
			want: "1d 00:00",
		},
		{
			name: "month-end 23h59m45s rolls into next month",
			t:    time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 2, 28, 23, 59, 45, 0, time.UTC),
			want: "1m 0d 00:00",
		},
		{
			name: "March 31 borrow uses February 28 days in non-leap year",
			t:    time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC),
			want: "28d 22:00",
		},
		{
			name: "March 31 borrow from Feb 2026",
			t:    time.Date(2026, 2, 28, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 3, 31, 10, 0, 0, 0, time.UTC),
			want: "1m 2d 22:00",
		},
		{
			name: "March 31 borrow from Feb 2024 leap year",
			t:    time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2024, 3, 31, 10, 0, 0, 0, time.UTC),
			want: "1m 1d 22:00",
		},
		{
			name: "cross-timezone UTC t and Zurich now",
			t:    time.Date(2026, 6, 15, 8, 0, 0, 0, time.UTC), // 10:00 CEST
			now:  time.Date(2026, 6, 15, 12, 30, 0, 0, zurich),
			want: "02:30",
		},
		{
			name: "multi-year duration",
			t:    time.Date(2024, 1, 10, 8, 15, 0, 0, time.UTC),
			now:  time.Date(2026, 4, 12, 12, 20, 0, 0, time.UTC),
			want: "2y 3m 2d 04:05",
		},
		{
			name: "January borrow wraps to December",
			t:    time.Date(2025, 12, 20, 15, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 1, 5, 10, 0, 0, 0, time.UTC),
			want: "15d 19:00",
		},
		{
			name: "DST fall-back 45 minutes elapsed",
			t:    time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC).In(zurich), // 02:30 CEST (+0200)
			now:  time.Date(2026, 10, 25, 1, 15, 0, 0, time.UTC).In(zurich), // 02:15 CET (+0100)
			want: "00:45",
		},
		{
			name: "DST fall-back 1 hour elapsed",
			t:    time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC).In(zurich), // 02:30 CEST (+0200)
			now:  time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC).In(zurich), // 02:30 CET (+0100)
			want: "01:00",
		},
		{
			name: "DST spring-forward 1 hour elapsed",
			t:    time.Date(2026, 3, 29, 0, 30, 0, 0, time.UTC).In(zurich), // 01:30 CET (+0100)
			now:  time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC).In(zurich), // 03:30 CEST (+0200)
			want: "01:00",
		},
		{
			name: "month-end Jan 31 to Mar 1 before noon has non-negative days",
			t:    time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC),
			want: "1m 0d 23:00",
		},
		{
			name: "month-end Jan 31 to Mar 1 at noon",
			t:    time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
			want: "1m 1d 00:00",
		},
		{
			name: "month-end Jan 31 to Feb 28 before noon",
			t:    time.Date(2026, 1, 31, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 2, 28, 11, 0, 0, 0, time.UTC),
			want: "27d 23:00",
		},
		{
			name: "sub-minute 29s rounds down to 00:00",
			t:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 6, 15, 12, 0, 29, 0, time.UTC),
			want: "00:00",
		},
		{
			name: "sub-minute 30s rounds up to 00:01",
			t:    time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2026, 6, 15, 12, 0, 30, 0, time.UTC),
			want: "00:01",
		},
		{
			name: "23h spring-forward day in Zurich",
			t:    time.Date(2026, 3, 29, 0, 0, 0, 0, zurich),
			now:  time.Date(2026, 3, 30, 0, 0, 0, 0, zurich),
			want: "23:00",
		},
		{
			name: "25h fall-back day in Zurich",
			t:    time.Date(2026, 10, 25, 0, 0, 0, 0, zurich),
			now:  time.Date(2026, 10, 26, 0, 0, 0, 0, zurich),
			want: "1d 01:00",
		},
		{
			name: "leap-day anniversary to non-leap Feb 28",
			t:    time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2025, 2, 28, 12, 0, 0, 0, time.UTC),
			want: "1y 0m 0d 00:00",
		},
		{
			name: "leap-day anniversary to next leap Feb 29",
			t:    time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC),
			now:  time.Date(2028, 2, 29, 12, 0, 0, 0, time.UTC),
			want: "4y 0m 0d 00:00",
		},
		{
			name: "negative year guards negative modulo",
			t:    time.Date(-1, 12, 15, 12, 0, 0, 0, time.UTC),
			now:  time.Date(0, 1, 15, 12, 0, 0, 0, time.UTC),
			want: "1m 0d 00:00",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatElapsed(tc.t, tc.now)
			if got != tc.want {
				t.Errorf("FormatElapsed(%v, %v) = %q, want %q", tc.t, tc.now, got, tc.want)
			}
		})
	}
}
