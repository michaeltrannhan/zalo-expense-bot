// Package summaryschedule implements P4-B03: optional daily, weekly and
// monthly summary delivery in each user's local timezone.
package summaryschedule

import (
	"fmt"
	"time"

	"zl-expese-bot/internal/domain"
)

// NextDelivery returns the first scheduled UTC instant strictly after now.
// Daily runs every day, weekly runs Monday, and monthly runs on the first.
func NextDelivery(now time.Time, loc *time.Location, frequency domain.SummaryFrequency, deliveryMinute int) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	if deliveryMinute < 0 || deliveryMinute >= 24*60 {
		return time.Time{}, fmt.Errorf("delivery minute must be between 0 and 1439")
	}
	hour, minute := deliveryMinute/60, deliveryMinute%60
	localNow := now.In(loc)
	y, m, d := localNow.Date()
	candidate := time.Date(y, m, d, hour, minute, 0, 0, loc)

	switch frequency {
	case domain.SummaryDaily:
		if !candidate.After(localNow) {
			candidate = candidate.AddDate(0, 0, 1)
		}
	case domain.SummaryWeekly:
		daysUntilMonday := (int(time.Monday) - int(candidate.Weekday()) + 7) % 7
		candidate = candidate.AddDate(0, 0, daysUntilMonday)
		if !candidate.After(localNow) {
			candidate = candidate.AddDate(0, 0, 7)
		}
	case domain.SummaryMonthly:
		candidate = time.Date(y, m, 1, hour, minute, 0, 0, loc)
		if !candidate.After(localNow) {
			candidate = candidate.AddDate(0, 1, 0)
		}
	default:
		return time.Time{}, fmt.Errorf("unsupported summary frequency %q", frequency)
	}
	return candidate.UTC(), nil
}

// LatestDelivery returns the most recent scheduled UTC instant at or before
// now. It lets a worker that was offline skip stale backlog and send only the
// latest completed period instead of flooding the user.
func LatestDelivery(now time.Time, loc *time.Location, frequency domain.SummaryFrequency, deliveryMinute int) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	if deliveryMinute < 0 || deliveryMinute >= 24*60 {
		return time.Time{}, fmt.Errorf("delivery minute must be between 0 and 1439")
	}
	hour, minute := deliveryMinute/60, deliveryMinute%60
	localNow := now.In(loc)
	y, m, d := localNow.Date()
	candidate := time.Date(y, m, d, hour, minute, 0, 0, loc)

	switch frequency {
	case domain.SummaryDaily:
		if candidate.After(localNow) {
			candidate = candidate.AddDate(0, 0, -1)
		}
	case domain.SummaryWeekly:
		daysSinceMonday := (int(candidate.Weekday()) + 6) % 7
		candidate = candidate.AddDate(0, 0, -daysSinceMonday)
		if candidate.After(localNow) {
			candidate = candidate.AddDate(0, 0, -7)
		}
	case domain.SummaryMonthly:
		candidate = time.Date(y, m, 1, hour, minute, 0, 0, loc)
		if candidate.After(localNow) {
			candidate = candidate.AddDate(0, -1, 0)
		}
	default:
		return time.Time{}, fmt.Errorf("unsupported summary frequency %q", frequency)
	}
	return candidate.UTC(), nil
}
