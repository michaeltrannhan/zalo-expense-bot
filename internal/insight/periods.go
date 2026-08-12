// Package insight computes spending summaries over user-timezone-aware
// periods and persists them as insights rows. Summaries always describe
// recorded spending (chi tiêu đã ghi nhận), never projected totals.
package insight

import "time"

// Period is a half-open interval [Start, End) in UTC. Bounds are derived
// from wall-clock midnights in the user's timezone, so adjacent periods
// share an edge and no instant is ever double-counted.
type Period struct {
	Start time.Time
	End   time.Time
}

// localMidnight returns the start of t's calendar day in loc.
func localMidnight(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// weekStartLocal returns Monday 00:00 of t's week in loc.
func weekStartLocal(t time.Time, loc *time.Location) time.Time {
	start := localMidnight(t, loc)
	back := (int(start.Weekday()) + 6) % 7 // days since Monday
	return start.AddDate(0, 0, -back)
}

// monthStartLocal returns the 1st 00:00 of t's month in loc.
func monthStartLocal(t time.Time, loc *time.Location) time.Time {
	y, m, _ := t.In(loc).Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, loc)
}

// Today returns the current calendar day in loc.
func Today(now time.Time, loc *time.Location) Period {
	start := localMidnight(now, loc)
	return Period{Start: start.UTC(), End: start.AddDate(0, 0, 1).UTC()}
}

// Yesterday returns the full calendar day before Today.
func Yesterday(now time.Time, loc *time.Location) Period {
	end := localMidnight(now, loc)
	start := end.AddDate(0, 0, -1)
	return Period{Start: start.UTC(), End: end.UTC()}
}

// ThisWeek returns the current week in loc; weeks start on Monday.
func ThisWeek(now time.Time, loc *time.Location) Period {
	start := weekStartLocal(now, loc)
	return Period{Start: start.UTC(), End: start.AddDate(0, 0, 7).UTC()}
}

// LastWeek returns the full week (Monday start) before ThisWeek.
func LastWeek(now time.Time, loc *time.Location) Period {
	start := weekStartLocal(now, loc).AddDate(0, 0, -7)
	return Period{Start: start.UTC(), End: start.AddDate(0, 0, 7).UTC()}
}

// ThisMonth returns the current calendar month in loc.
func ThisMonth(now time.Time, loc *time.Location) Period {
	start := monthStartLocal(now, loc)
	return Period{Start: start.UTC(), End: start.AddDate(0, 1, 0).UTC()}
}

// LastMonth returns the full calendar month before ThisMonth.
func LastMonth(now time.Time, loc *time.Location) Period {
	start := monthStartLocal(now, loc).AddDate(0, -1, 0)
	return Period{Start: start.UTC(), End: start.AddDate(0, 1, 0).UTC()}
}
