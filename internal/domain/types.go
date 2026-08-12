// Package domain holds the provider-neutral core model of the application.
// No package under internal/ may import provider SDK types; adapters live at
// the edges (internal/messaging/zalo, internal/extraction/mock).
package domain

import (
	"time"

	"github.com/google/uuid"
)

// Provider identifies the messaging platform a record originates from.
type Provider string

const (
	ProviderZaloBot Provider = "zalo_bot"
)

// Event types mirrored from the Zalo Bot webhook contract.
const (
	EventTextReceived  = "message.text.received"
	EventImageReceived = "message.image.received"
	EventUnsupported   = "message.unsupported.received"
)

type UserStatus string

const (
	UserPending   UserStatus = "pending"   // seen, has not consented yet
	UserActive    UserStatus = "active"    // consent recorded
	UserSuspended UserStatus = "suspended" // administratively blocked
	UserDeleted   UserStatus = "deleted"   // data deletion executed
)

type User struct {
	ID              uuid.UUID
	Status          UserStatus
	Timezone        string // IANA name, e.g. "Asia/Ho_Chi_Minh"
	DefaultCurrency string // ISO 4217
	Locale          string
	ConsentVersion  string
	ConsentedAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
	DeletedAt       *time.Time
}

// UserIdentity maps a provider-issued subject onto a local user. Identity is
// always the provider sender ID, never a display name.
type UserIdentity struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Provider        Provider
	ProviderSubject string
	ProviderScope   string // bot ID, so one sender can exist across bots
	CreatedAt       time.Time
	LastSeenAt      time.Time
}

type MessageStatus string

const (
	MessageReceived  MessageStatus = "received"
	MessageProcessed MessageStatus = "processed"
	MessageDuplicate MessageStatus = "duplicate"
	MessageFailed    MessageStatus = "failed"
)

// ProviderMessage is the idempotency anchor for inbound webhooks.
type ProviderMessage struct {
	ID                  uuid.UUID
	Provider            Provider
	ProviderChatID      string
	ProviderMessageID   string
	UserID              *uuid.UUID
	EventType           string
	PayloadHash         string
	RawPayload          []byte // JSONB; redacted in logs, retention-limited
	ReceivedAt          time.Time
	ProcessingStartedAt *time.Time
	ProcessedAt         *time.Time
	DeleteAfter         *time.Time
	Status              MessageStatus
}

type ReceiptStatus string

const (
	ReceiptReceived        ReceiptStatus = "received"
	ReceiptQueued          ReceiptStatus = "queued"
	ReceiptDownloading     ReceiptStatus = "downloading"
	ReceiptStored          ReceiptStatus = "stored"
	ReceiptExtracting      ReceiptStatus = "extracting"
	ReceiptReviewRequired  ReceiptStatus = "review_required"
	ReceiptConfirmed       ReceiptStatus = "confirmed"
	ReceiptDeleted         ReceiptStatus = "deleted"
	ReceiptFailedTransient ReceiptStatus = "failed_transient"
	ReceiptFailedPermanent ReceiptStatus = "failed_permanent"
)

type ReceiptDocument struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	ProviderMessageID *uuid.UUID
	StorageKey        string
	SourceURLHash     string
	ContentType       string
	ByteSize          int64
	SHA256            string
	PerceptualHash    string
	Status            ReceiptStatus
	RetentionPolicy   string // e.g. "originals_30d", "delete_after_extraction"
	DeleteAfter       *time.Time
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

type TxType string

const (
	TxExpense    TxType = "expense"
	TxIncome     TxType = "income"
	TxRefund     TxType = "refund"
	TxTransfer   TxType = "transfer"
	TxAdjustment TxType = "adjustment"
)

type TxStatus string

const (
	TxDraft                TxStatus = "draft"
	TxAwaitingConfirmation TxStatus = "awaiting_confirmation"
	TxConfirmed            TxStatus = "confirmed"
	TxAmended              TxStatus = "amended"
	TxDeleted              TxStatus = "deleted"
)

type TxSource string

const (
	TxSourceReceipt TxSource = "receipt"
	TxSourceManual  TxSource = "manual"
)

type Transaction struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	ReceiptDocumentID *uuid.UUID
	AccountID         *uuid.UUID
	Type              TxType
	MerchantID        *uuid.UUID
	MerchantName      string // display copy; merchant_id is the join
	Description       string
	AmountMinor       int64 // minor units; NEVER float money
	Currency          string
	OccurredAt        time.Time // UTC
	CategoryID        *uuid.UUID
	Status            TxStatus
	Source            TxSource
	ConfidenceSummary string // compact human/JSON summary, e.g. "total:0.98,date:0.90"
	ConfirmedAt       *time.Time
	Version           int // optimistic concurrency
	CreatedAt         time.Time
	UpdatedAt         time.Time
	DeletedAt         *time.Time
}

// ExtractedField is one OCR/extraction output with provenance.
type ExtractedField struct {
	ID                uuid.UUID
	ReceiptDocumentID uuid.UUID
	FieldName         string // merchant|total_minor|currency|occurred_at|...
	RawValue          string
	NormalisedValue   string
	Confidence        float64
	Source            string // extractor|normaliser|user_rule|default
	Evidence          []byte // JSONB
	ExtractorName     string
	ExtractorVersion  string
	CreatedAt         time.Time
}

// Prediction records what the system guessed so corrections can be audited
// and learning rules can be trained. prediction_type: type|category|merchant.
type Prediction struct {
	ID              uuid.UUID
	TransactionID   uuid.UUID
	PredictionType  string
	PredictedValue  string
	Confidence      float64
	ModelName       string
	ModelVersion    string
	FeatureSnapshot []byte // JSONB
	CreatedAt       time.Time
}

// Correction is a first-class user fix; it feeds user_merchant_rules.
type Correction struct {
	ID             uuid.UUID
	TransactionID  uuid.UUID
	FieldName      string
	PredictedValue string
	CorrectedValue string
	Source         string // chat_confirm|chat_edit|chat_discard|manual
	CreatedAt      time.Time
}

type Merchant struct {
	ID             uuid.UUID
	CanonicalName  string
	NormalisedName string
	MerchantFamily string
	CreatedAt      time.Time
}

type MerchantAlias struct {
	ID              uuid.UUID
	MerchantID      uuid.UUID
	Alias           string
	NormalisedAlias string
	Source          string // seed|extraction|user
}

// UserMerchantRule is the learned P(category|user,merchant) preference,
// count-based on purpose (plan: no custom neural model in MVP).
type UserMerchantRule struct {
	UserID                   uuid.UUID
	MerchantID               uuid.UUID
	PreferredCategoryID      *uuid.UUID
	PreferredTransactionType TxType
	SampleCount              int
	Confidence               float64
	LastConfirmedAt          time.Time
}

type Category struct {
	ID          uuid.UUID
	SystemKey   string // stable machine key, e.g. "an-uong"
	DisplayName string // Vietnamese display name
	ParentID    *uuid.UUID
	IsSystem    bool
	CreatedAt   time.Time
}

type InsightStatus string

const (
	InsightReady     InsightStatus = "ready"
	InsightDismissed InsightStatus = "dismissed"
)

type Insight struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	InsightType      string // daily|weekly|monthly
	PeriodStart      time.Time
	PeriodEnd        time.Time
	Payload          []byte // JSONB, rendered numbers
	Evidence         []byte // JSONB, supporting transaction IDs + method
	GeneratorVersion string
	Status           InsightStatus
	CreatedAt        time.Time
	DismissedAt      *time.Time
}

// SummaryFrequency is one opt-in scheduled insight cadence. Weekly
// summaries run on Monday for the completed week; monthly summaries run on
// the first day for the completed month.
type SummaryFrequency string

const (
	SummaryDaily   SummaryFrequency = "daily"
	SummaryWeekly  SummaryFrequency = "weekly"
	SummaryMonthly SummaryFrequency = "monthly"
)

// SummarySchedule stores one user's delivery preference. DeliveryMinute is
// the local wall-clock minute (hour*60 + minute); delivery instants are UTC.
type SummarySchedule struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Frequency       SummaryFrequency
	DeliveryMinute  int
	Provider        Provider
	ProviderChatID  string
	Enabled         bool
	NextDeliveryAt  time.Time
	LastDeliveredAt *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// UsageCounter powers quota enforcement and the 70/85/95% warnings.
// Period format: "2026-07" for monthly, "2026-07-19" for daily.
type UsageCounter struct {
	Scope     string // global|user
	ScopeID   string // "" for global, user UUID otherwise
	Period    string
	Metric    string // zalo_messages_sent|zalo_messages_received|ocr_pages|images_downloaded|ocr_failures|receipts_processed
	Count     int64
	Limit     int64
	UpdatedAt time.Time
}

type JobKind string

const (
	JobReceiptProcess JobKind = "receipt_process"
	JobOutboundSend   JobKind = "outbound_send"
	JobRetentionSweep JobKind = "retention_sweep"
)

type JobStatus string

const (
	JobQueued  JobStatus = "queued"
	JobRunning JobStatus = "running"
	JobDone    JobStatus = "done"
	JobDead    JobStatus = "dead" // exhausted retries; surfaced for inspection
)

// QueueJob is the local PostgreSQL-backed queue row (SQS replacement).
type QueueJob struct {
	ID          uuid.UUID
	Kind        JobKind
	Payload     []byte // JSONB; contracts/events schemas
	DedupeKey   *string
	Status      JobStatus
	Attempts    int
	MaxAttempts int
	RunAfter    time.Time
	VisibleAt   time.Time // visibility timeout deadline
	LastError   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type PendingKind string

const (
	PendingConfirmExtraction PendingKind = "confirm_extraction"
	PendingEditTotal         PendingKind = "edit_total"
	PendingEditMerchant      PendingKind = "edit_merchant"
	PendingEditDate          PendingKind = "edit_date"
	PendingEditCategory      PendingKind = "edit_category"
	PendingEditType          PendingKind = "edit_type"
	PendingDeleteAccount     PendingKind = "delete_account"
	PendingDeleteRecent      PendingKind = "delete_recent"
)

// PendingAction is the server-side chat state machine slot: the next user
// text message is interpreted against this row. One open row per user.
type PendingAction struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	Kind          PendingKind
	TransactionID *uuid.UUID
	Payload       []byte // JSONB, kind-specific (e.g. offered category list)
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

type OutboundStatus string

const (
	OutboundQueued     OutboundStatus = "queued"
	OutboundSending    OutboundStatus = "sending"
	OutboundSent       OutboundStatus = "sent"
	OutboundFailed     OutboundStatus = "failed"
	OutboundSuppressed OutboundStatus = "suppressed" // quota kill-switch
	OutboundAmbiguous  OutboundStatus = "ambiguous"  // provider may have accepted the attempt
)

// OutboundRecord is the idempotent outbox row the notification worker sends.
type OutboundRecord struct {
	ID                uuid.UUID
	UserID            uuid.UUID
	Provider          Provider
	ProviderChatID    string
	IdempotencyKey    string // e.g. "confirm-card:<txID>:v3"
	Body              string
	Ephemeral         bool
	Status            OutboundStatus
	Attempts          int
	LastError         string
	AttemptedAt       *time.Time
	SentAt            *time.Time
	ProviderMessageID string
	CreatedAt         time.Time
}
