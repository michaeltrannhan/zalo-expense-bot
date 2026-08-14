// Package account implements self-service data rights: portable export of
// a user's own data (CSV/JSON) and full account deletion.
package account

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"

	"zl-expese-bot/internal/domain"
	"zl-expese-bot/internal/store"
)

var csvHeader = []string{
	"id", "occurred_at", "type", "merchant", "description",
	"amount_minor", "currency", "category_key", "status", "source",
}

// ExportTransactionsCSV writes every confirmed/amended, non-deleted
// transaction belonging to userID to w as CSV, ordered by occurred_at
// (then id). Timestamps are RFC3339 UTC and amounts are integer minor
// units. It returns the number of data rows written (header excluded).
func ExportTransactionsCSV(ctx context.Context, st *store.Store, userID uuid.UUID, w io.Writer) (int, error) {
	rows, err := st.ListExportTransactions(ctx, userID)
	if err != nil {
		return 0, err
	}

	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return 0, domain.E(domain.CodeTransient, "write csv header", err)
	}
	n := 0
	for _, row := range rows {
		rec := []string{
			row.ID.String(),
			row.OccurredAt.UTC().Format(time.RFC3339),
			row.Type, row.Merchant, row.Description,
			strconv.FormatInt(row.AmountMinor, 10),
			row.Currency, row.CategoryKey, row.Status, row.Source,
		}
		if err := cw.Write(rec); err != nil {
			return n, domain.E(domain.CodeTransient, "write csv row", err)
		}
		n++
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return n, domain.E(domain.CodeTransient, "flush csv", err)
	}
	return n, nil
}

// MetadataExport is the JSON account-metadata document: the user's own row,
// consent record, linked provider identities and record counts.
type MetadataExport struct {
	ExportedAt       time.Time         `json:"exported_at"`
	User             MetadataUser      `json:"user"`
	Consent          Consent           `json:"consent"`
	Identities       []Identity        `json:"identities"`
	SummarySchedules []SummarySchedule `json:"summary_schedules"`
	Counts           Counts            `json:"counts"`
}

// MetadataUser mirrors the users row; it contains only the user's own data.
type MetadataUser struct {
	ID              uuid.UUID  `json:"id"`
	Status          string     `json:"status"`
	Timezone        string     `json:"timezone"`
	DefaultCurrency string     `json:"default_currency"`
	Locale          string     `json:"locale"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	DeletedAt       *time.Time `json:"deleted_at,omitempty"`
}

// Consent records which consent text version the user accepted, and when.
type Consent struct {
	Version     string     `json:"version"`
	ConsentedAt *time.Time `json:"consented_at,omitempty"`
}

// Identity is one linked provider subject (e.g. a Zalo sender ID).
type Identity struct {
	Provider        string `json:"provider"`
	ProviderSubject string `json:"provider_subject"`
	ProviderScope   string `json:"provider_scope"`
}

// SummarySchedule is one user-controlled automatic-summary preference.
type SummarySchedule struct {
	Frequency       string     `json:"frequency"`
	DeliveryMinute  int        `json:"delivery_minute"`
	Provider        string     `json:"provider"`
	ProviderChatID  string     `json:"provider_chat_id"`
	Enabled         bool       `json:"enabled"`
	NextDeliveryAt  time.Time  `json:"next_delivery_at"`
	LastDeliveredAt *time.Time `json:"last_delivered_at,omitempty"`
}

// Counts tallies the user's non-deleted records by table.
type Counts struct {
	Transactions     int `json:"transactions"`
	Receipts         int `json:"receipts"`
	Insights         int `json:"insights"`
	SummarySchedules int `json:"summary_schedules"`
}

// ExportMetadataJSON writes the user's account metadata to w as indented
// JSON. It returns domain.CodeNotFound when the user does not exist.
func ExportMetadataJSON(ctx context.Context, st *store.Store, userID uuid.UUID, w io.Writer) error {
	meta, err := st.LoadAccountExport(ctx, userID)
	if err != nil {
		return err
	}

	doc := MetadataExport{
		ExportedAt: time.Now().UTC(),
		User: MetadataUser{
			ID: meta.User.ID, Status: string(meta.User.Status),
			Timezone: meta.User.Timezone, DefaultCurrency: meta.User.DefaultCurrency,
			Locale: meta.User.Locale, CreatedAt: meta.User.CreatedAt,
			UpdatedAt: meta.User.UpdatedAt, DeletedAt: meta.User.DeletedAt,
		},
		Consent: Consent{
			Version:     meta.User.ConsentVersion,
			ConsentedAt: meta.User.ConsentedAt,
		},
		Identities:       []Identity{},
		SummarySchedules: []SummarySchedule{},
		Counts: Counts{
			Transactions:     meta.TransactionCount,
			Receipts:         meta.ReceiptCount,
			Insights:         meta.InsightCount,
			SummarySchedules: meta.ScheduleCount,
		},
	}
	for _, ident := range meta.Identities {
		doc.Identities = append(doc.Identities, Identity{
			Provider:        string(ident.Provider),
			ProviderSubject: ident.ProviderSubject,
			ProviderScope:   ident.ProviderScope,
		})
	}
	for _, preference := range meta.SummarySchedules {
		doc.SummarySchedules = append(doc.SummarySchedules, SummarySchedule{
			Frequency:       string(preference.Frequency),
			DeliveryMinute:  preference.DeliveryMinute,
			Provider:        string(preference.Provider),
			ProviderChatID:  preference.ProviderChatID,
			Enabled:         preference.Enabled,
			NextDeliveryAt:  preference.NextDeliveryAt,
			LastDeliveredAt: preference.LastDeliveredAt,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return domain.E(domain.CodeTransient, "encode metadata export", err)
	}
	return nil
}
