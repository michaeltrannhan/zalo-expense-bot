package summaryschedule

import (
	"testing"
	"time"

	"zl-expese-bot/internal/domain"
)

func TestNextDelivery(t *testing.T) {
	hcm, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		now       string
		frequency domain.SummaryFrequency
		minute    int
		want      string
	}{
		{"daily later today", "2026-07-23T10:00:00+07:00", domain.SummaryDaily, 20 * 60, "2026-07-23T20:00:00+07:00"},
		{"daily tomorrow", "2026-07-23T20:00:00+07:00", domain.SummaryDaily, 20 * 60, "2026-07-24T20:00:00+07:00"},
		{"weekly next monday", "2026-07-23T10:00:00+07:00", domain.SummaryWeekly, 8*60 + 30, "2026-07-27T08:30:00+07:00"},
		{"weekly monday later", "2026-07-27T07:00:00+07:00", domain.SummaryWeekly, 8*60 + 30, "2026-07-27T08:30:00+07:00"},
		{"monthly next first", "2026-07-23T10:00:00+07:00", domain.SummaryMonthly, 9 * 60, "2026-08-01T09:00:00+07:00"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, _ := time.Parse(time.RFC3339, tt.now)
			want, _ := time.Parse(time.RFC3339, tt.want)
			got, err := NextDelivery(now, hcm, tt.frequency, tt.minute)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(want) {
				t.Fatalf("NextDelivery = %s, want %s", got, want)
			}
		})
	}
}

func TestLatestDeliverySkipsBacklog(t *testing.T) {
	hcm, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	now, _ := time.Parse(time.RFC3339, "2026-07-23T21:15:00+07:00")
	got, err := LatestDelivery(now, hcm, domain.SummaryDaily, 20*60)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := time.Parse(time.RFC3339, "2026-07-23T20:00:00+07:00")
	if !got.Equal(want) {
		t.Fatalf("LatestDelivery = %s, want %s", got, want)
	}
}

func TestDeliveryValidation(t *testing.T) {
	if _, err := NextDelivery(time.Now(), time.UTC, domain.SummaryDaily, 1440); err == nil {
		t.Fatal("expected invalid delivery minute error")
	}
	if _, err := LatestDelivery(time.Now(), time.UTC, "yearly", 0); err == nil {
		t.Fatal("expected unsupported frequency error")
	}
}
