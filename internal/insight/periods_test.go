package insight

import (
	"testing"
	"time"
)

var hcm = func() *time.Location {
	l, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		panic(err)
	}
	return l
}()

func utc(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestPeriods(t *testing.T) {
	sydney, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		fn        func(time.Time, *time.Location) Period
		now       time.Time
		loc       *time.Location
		wantStart string
		wantEnd   string
	}{
		// Today: local day boundaries in Asia/Ho_Chi_Minh (UTC+7, no DST).
		{"today midday", Today, utc("2026-07-19T03:00:00Z"), hcm,
			"2026-07-18T17:00:00Z", "2026-07-19T17:00:00Z"},
		{"today just before local midnight", Today, utc("2026-07-18T16:59:59Z"), hcm,
			"2026-07-17T17:00:00Z", "2026-07-18T17:00:00Z"},
		{"today exactly at local midnight", Today, utc("2026-07-18T17:00:00Z"), hcm,
			"2026-07-18T17:00:00Z", "2026-07-19T17:00:00Z"},
		{"today month boundary", Today, utc("2026-07-31T17:00:00Z"), hcm,
			"2026-07-31T17:00:00Z", "2026-08-01T17:00:00Z"},
		{"today year boundary", Today, utc("2025-12-31T17:00:00Z"), hcm,
			"2025-12-31T17:00:00Z", "2026-01-01T17:00:00Z"},
		{"today leap day", Today, utc("2024-02-29T05:00:00Z"), hcm,
			"2024-02-28T17:00:00Z", "2024-02-29T17:00:00Z"},
		{"today utc zone", Today, utc("2026-07-19T05:00:00Z"), time.UTC,
			"2026-07-19T00:00:00Z", "2026-07-20T00:00:00Z"},
		{"yesterday", Yesterday, utc("2026-07-19T03:00:00Z"), hcm,
			"2026-07-17T17:00:00Z", "2026-07-18T17:00:00Z"},
		// DST spring-forward day in Sydney is 23h; bounds stay wall-clock
		// midnights.
		{"today dst spring forward", Today, utc("2025-10-05T04:00:00Z"), sydney,
			"2025-10-04T14:00:00Z", "2025-10-05T13:00:00Z"},
		{"today dst fall back", Today, utc("2026-04-05T03:00:00Z"), sydney,
			"2026-04-04T13:00:00Z", "2026-04-05T14:00:00Z"},

		// ThisWeek: weeks start Monday. End is clipped to local midnight
		// tomorrow so future weekdays are not counted as no-spend.
		{"thisweek sunday", ThisWeek, utc("2026-07-19T03:00:00Z"), hcm,
			"2026-07-12T17:00:00Z", "2026-07-19T17:00:00Z"},
		{"thisweek monday at local midnight", ThisWeek, utc("2026-07-12T17:00:00Z"), hcm,
			"2026-07-12T17:00:00Z", "2026-07-13T17:00:00Z"},
		{"thisweek monday one second before", ThisWeek, utc("2026-07-12T16:59:59Z"), hcm,
			"2026-07-05T17:00:00Z", "2026-07-12T17:00:00Z"},
		{"thisweek wednesday", ThisWeek, utc("2026-07-15T10:00:00Z"), hcm,
			"2026-07-12T17:00:00Z", "2026-07-15T17:00:00Z"},
		{"thisweek across year", ThisWeek, utc("2026-01-01T05:00:00Z"), hcm,
			"2025-12-28T17:00:00Z", "2026-01-01T17:00:00Z"},
		{"thisweek across month", ThisWeek, utc("2026-06-01T02:00:00Z"), hcm,
			"2026-05-31T17:00:00Z", "2026-06-01T17:00:00Z"},

		// LastWeek.
		{"lastweek sunday", LastWeek, utc("2026-07-19T03:00:00Z"), hcm,
			"2026-07-05T17:00:00Z", "2026-07-12T17:00:00Z"},
		{"lastweek across year", LastWeek, utc("2026-01-01T05:00:00Z"), hcm,
			"2025-12-21T17:00:00Z", "2025-12-28T17:00:00Z"},
		{"lastweek spans leap day", LastWeek, utc("2024-03-04T01:00:00Z"), hcm,
			"2024-02-25T17:00:00Z", "2024-03-03T17:00:00Z"},

		// ThisMonth: End clipped to local midnight tomorrow.
		{"thismonth mid january", ThisMonth, utc("2026-01-15T05:00:00Z"), hcm,
			"2025-12-31T17:00:00Z", "2026-01-15T17:00:00Z"},
		{"thismonth first instant of month", ThisMonth, utc("2026-07-31T17:00:00Z"), hcm,
			"2026-07-31T17:00:00Z", "2026-08-01T17:00:00Z"},
		{"thismonth last instant of month", ThisMonth, utc("2026-08-31T16:59:59Z"), hcm,
			"2026-07-31T17:00:00Z", "2026-08-31T17:00:00Z"},
		{"thismonth february non-leap", ThisMonth, utc("2026-02-10T05:00:00Z"), hcm,
			"2026-01-31T17:00:00Z", "2026-02-10T17:00:00Z"},
		{"thismonth february leap", ThisMonth, utc("2024-02-10T05:00:00Z"), hcm,
			"2024-01-31T17:00:00Z", "2024-02-10T17:00:00Z"},

		// LastMonth.
		{"lastmonth from january crosses year", LastMonth, utc("2026-01-15T05:00:00Z"), hcm,
			"2025-11-30T17:00:00Z", "2025-12-31T17:00:00Z"},
		{"lastmonth from march leap year", LastMonth, utc("2024-03-01T05:00:00Z"), hcm,
			"2024-01-31T17:00:00Z", "2024-02-29T17:00:00Z"},
		{"lastmonth from march non-leap", LastMonth, utc("2026-03-01T05:00:00Z"), hcm,
			"2026-01-31T17:00:00Z", "2026-02-28T17:00:00Z"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.fn(tc.now, tc.loc)
			if want := utc(tc.wantStart); !got.Start.Equal(want) {
				t.Errorf("Start = %s, want %s", got.Start, want)
			}
			if want := utc(tc.wantEnd); !got.End.Equal(want) {
				t.Errorf("End = %s, want %s", got.End, want)
			}
			if got.Start.Location() != time.UTC || got.End.Location() != time.UTC {
				t.Errorf("bounds must be in UTC, got %s / %s",
					got.Start.Location(), got.End.Location())
			}
			// Bounds are wall-clock midnights in the user's location.
			if h, m, s := got.Start.In(tc.loc).Clock(); h != 0 || m != 0 || s != 0 {
				t.Errorf("Start %s is not local midnight in %s", got.Start, tc.loc)
			}
		})
	}
}

// Adjacent periods must tile the timeline with no gaps and no overlaps:
// every End equals the successor's Start (half-open, no midnight double
// counting). Current ThisWeek/ThisMonth ends are clipped to tomorrow, so
// they do not extend to the next calendar week/month edge.
func TestPeriodsAreHalfOpen(t *testing.T) {
	now := utc("2026-07-19T03:00:00Z")

	today := Today(now, hcm)
	if next := Today(today.End, hcm); !next.Start.Equal(today.End) {
		t.Errorf("Today end %s != next day start %s", today.End, next.Start)
	}

	thisW, lastW := ThisWeek(now, hcm), LastWeek(now, hcm)
	if !lastW.End.Equal(thisW.Start) {
		t.Errorf("LastWeek end %s != ThisWeek start %s", lastW.End, thisW.Start)
	}
	tomorrow := localMidnight(now, hcm).AddDate(0, 0, 1).UTC()
	if !thisW.End.Equal(tomorrow) {
		t.Errorf("ThisWeek end %s != local midnight tomorrow %s", thisW.End, tomorrow)
	}

	thisM, lastM := ThisMonth(now, hcm), LastMonth(now, hcm)
	if !lastM.End.Equal(thisM.Start) {
		t.Errorf("LastMonth end %s != ThisMonth start %s", lastM.End, thisM.Start)
	}
	if !thisM.End.Equal(tomorrow) {
		t.Errorf("ThisMonth end %s != local midnight tomorrow %s", thisM.End, tomorrow)
	}
}
