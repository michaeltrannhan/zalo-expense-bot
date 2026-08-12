// Package account implements self-service data rights: portable export of
// a user's own data (CSV/JSON) and full account deletion.
package account

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"zl-expese-bot/internal/domain"
)

var csvHeader = []string{
	"id", "occurred_at", "type", "merchant", "description",
	"amount_minor", "currency", "category_key", "status", "source",
}

// ExportTransactionsCSV writes every non-deleted transaction belonging to
// userID to w as CSV, ordered by occurred_at (then id). Timestamps are
// RFC3339 UTC and amounts are integer minor units. It returns the number of
// data rows written (header excluded).
func ExportTransactionsCSV(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, w io.Writer) (int, error) {
	rows, err := pool.Query(ctx, `
		SELECT t.id, t.occurred_at, t.type, t.merchant_name, t.description,
		       t.amount_minor, t.currency, COALESCE(c.system_key, ''), t.status, t.source
		FROM transactions t
		LEFT JOIN categories c ON c.id = t.category_id
		WHERE t.user_id = $1 AND t.deleted_at IS NULL
		ORDER BY t.occurred_at ASC, t.id ASC`, userID)
	if err != nil {
		return 0, domain.E(domain.CodeTransient, "query transactions for export", err)
	}
	defer rows.Close()

	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader); err != nil {
		return 0, domain.E(domain.CodeTransient, "write csv header", err)
	}
	n := 0
	for rows.Next() {
		var (
			id          uuid.UUID
			occurredAt  time.Time
			amountMinor int64
			txType      string
			merchant    string
			description string
			currency    string
			categoryKey string
			status      string
			source      string
		)
		if err := rows.Scan(&id, &occurredAt, &txType, &merchant, &description,
			&amountMinor, &currency, &categoryKey, &status, &source); err != nil {
			return n, domain.E(domain.CodeTransient, "scan transaction export row", err)
		}
		rec := []string{
			id.String(),
			occurredAt.UTC().Format(time.RFC3339),
			txType, merchant, description,
			strconv.FormatInt(amountMinor, 10),
			currency, categoryKey, status, source,
		}
		if err := cw.Write(rec); err != nil {
			return n, domain.E(domain.CodeTransient, "write csv row", err)
		}
		n++
	}
	if err := rows.Err(); err != nil {
		return n, domain.E(domain.CodeTransient, "iterate transactions export", err)
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
func ExportMetadataJSON(ctx context.Context, pool *pgxpool.Pool, userID uuid.UUID, w io.Writer) error {
	doc := MetadataExport{
		ExportedAt:       time.Now().UTC(),
		Identities:       []Identity{},
		SummarySchedules: []SummarySchedule{},
	}

	err := pool.QueryRow(ctx, `
		SELECT id, status, timezone, default_currency, locale,
		       consent_version, consented_at, created_at, updated_at, deleted_at
		FROM users WHERE id = $1`, userID).
		Scan(&doc.User.ID, &doc.User.Status, &doc.User.Timezone,
			&doc.User.DefaultCurrency, &doc.User.Locale,
			&doc.Consent.Version, &doc.Consent.ConsentedAt,
			&doc.User.CreatedAt, &doc.User.UpdatedAt, &doc.User.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Ef(domain.CodeNotFound, nil, "user %s not found", userID)
	}
	if err != nil {
		return domain.E(domain.CodeTransient, "load user for metadata export", err)
	}

	rows, err := pool.Query(ctx, `
		SELECT provider, provider_subject, provider_scope
		FROM user_identities WHERE user_id = $1
		ORDER BY provider, provider_subject`, userID)
	if err != nil {
		return domain.E(domain.CodeTransient, "query identities for metadata export", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id Identity
		if err := rows.Scan(&id.Provider, &id.ProviderSubject, &id.ProviderScope); err != nil {
			return domain.E(domain.CodeTransient, "scan identity export row", err)
		}
		doc.Identities = append(doc.Identities, id)
	}
	if err := rows.Err(); err != nil {
		return domain.E(domain.CodeTransient, "iterate identities export", err)
	}

	scheduleRows, err := pool.Query(ctx, `
		SELECT frequency, delivery_minute, provider, provider_chat_id,
		       enabled, next_delivery_at, last_delivered_at
		FROM scheduled_summary_preferences
		WHERE user_id = $1
		ORDER BY frequency`, userID)
	if err != nil {
		return domain.E(domain.CodeTransient, "query summary schedules for metadata export", err)
	}
	defer scheduleRows.Close()
	for scheduleRows.Next() {
		var preference SummarySchedule
		if err := scheduleRows.Scan(&preference.Frequency, &preference.DeliveryMinute,
			&preference.Provider, &preference.ProviderChatID, &preference.Enabled,
			&preference.NextDeliveryAt, &preference.LastDeliveredAt); err != nil {
			return domain.E(domain.CodeTransient, "scan summary schedule export row", err)
		}
		doc.SummarySchedules = append(doc.SummarySchedules, preference)
	}
	if err := scheduleRows.Err(); err != nil {
		return domain.E(domain.CodeTransient, "iterate summary schedule export", err)
	}

	err = pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*)::int FROM transactions
		    WHERE user_id = $1 AND deleted_at IS NULL),
		  (SELECT COUNT(*)::int FROM receipt_documents
		    WHERE user_id = $1 AND deleted_at IS NULL),
		  (SELECT COUNT(*)::int FROM insights WHERE user_id = $1),
		  (SELECT COUNT(*)::int FROM scheduled_summary_preferences
		    WHERE user_id = $1)`, userID).
		Scan(&doc.Counts.Transactions, &doc.Counts.Receipts, &doc.Counts.Insights,
			&doc.Counts.SummarySchedules)
	if err != nil {
		return domain.E(domain.CodeTransient, "count records for metadata export", err)
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return domain.E(domain.CodeTransient, "encode metadata export", err)
	}
	return nil
}
