// Package events defines the versioned cross-process contracts (plan §10).
// Schema version 1 is frozen for the MVP; changes require a new
// schema_version and a contract test update.
package events

import (
	"time"

	"github.com/google/uuid"
)

// SchemaV1 is the only supported contract version in the MVP.
const SchemaV1 = 1

// InboundEvent is the normalised form of any provider webhook (§10.1).
type InboundEvent struct {
	Provider          string           `json:"provider"`
	ProviderUserID    string           `json:"provider_user_id"`
	ProviderChatID    string           `json:"provider_chat_id"`
	ProviderMessageID string           `json:"provider_message_id"`
	EventType         string           `json:"event_type"`
	Text              string           `json:"text,omitempty"`
	Media             []MediaReference `json:"media,omitempty"`
	ReceivedAt        time.Time        `json:"received_at"`
	RawPayloadHash    string           `json:"raw_payload_hash"`
	RawPayload        []byte           `json:"-"` // stored in provider_messages only, never queued
}

// MediaReference points at provider-hosted media. Image bytes never travel
// through the queue (§10.2).
type MediaReference struct {
	Provider string `json:"provider"`
	URL      string `json:"url"`
	MimeType string `json:"mime_type,omitempty"`
	Caption  string `json:"caption,omitempty"`
}

// ReceiptJob is the receipt_process queue payload (§10.3).
type ReceiptJob struct {
	SchemaVersion     int       `json:"schema_version"`
	JobID             uuid.UUID `json:"job_id"`
	ReceiptID         uuid.UUID `json:"receipt_id"`
	UserID            uuid.UUID `json:"user_id"`
	Provider          string    `json:"provider"`
	ProviderChatID    string    `json:"provider_chat_id"`
	ProviderMessageID string    `json:"provider_message_id"`
	Attempt           int       `json:"attempt"`
	EnqueuedAt        time.Time `json:"enqueued_at"`
}

// ExtractorInfo identifies the code that produced an ExtractionResult.
type ExtractorInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// FieldValue is one extracted field with provenance (§10.4).
type FieldValue struct {
	Raw        string  `json:"raw"`
	Normalised any     `json:"normalised"`
	Confidence float64 `json:"confidence"`
}

// LineItem is an optional itemised receipt row.
type LineItem struct {
	Name        string `json:"name"`
	Quantity    int    `json:"quantity,omitempty"`
	AmountMinor int64  `json:"amount_minor"`
}

// ExtractionResult is the extractor output consumed by normalisation (§10.4).
// Field keys: merchant, total_minor, currency, occurred_at, type_hint.
type ExtractionResult struct {
	SchemaVersion int                   `json:"schema_version"`
	Extractor     ExtractorInfo         `json:"extractor"`
	Fields        map[string]FieldValue `json:"fields"`
	LineItems     []LineItem            `json:"line_items"`
	Warnings      []string              `json:"warnings"`
}

// OutboundJob is the outbound_send queue payload.
type OutboundJob struct {
	SchemaVersion int       `json:"schema_version"`
	OutboundID    uuid.UUID `json:"outbound_id"`
	UserID        uuid.UUID `json:"user_id"`
	EnqueuedAt    time.Time `json:"enqueued_at"`
}

// RetentionSweepJob is the retention_sweep queue payload: a periodic hint
// to delete receipt originals whose delete_after deadline has passed (§12).
// It carries no identifiers — the sweep re-queries the database, so replay
// and duplication are naturally idempotent.
type RetentionSweepJob struct {
	SchemaVersion int       `json:"schema_version"`
	EnqueuedAt    time.Time `json:"enqueued_at"`
}
