-- Zalo Expense Tracker core schema (plan §8). Money is BIGINT minor units,
-- timestamps are UTC, idempotency lives in UNIQUE constraints.

CREATE TABLE users (
    id               UUID PRIMARY KEY,
    status           TEXT NOT NULL DEFAULT 'pending'
                     CHECK (status IN ('pending','active','suspended','deleted')),
    timezone         TEXT NOT NULL DEFAULT 'Asia/Ho_Chi_Minh',
    default_currency TEXT NOT NULL DEFAULT 'VND',
    locale           TEXT NOT NULL DEFAULT 'vi-VN',
    consent_version  TEXT NOT NULL DEFAULT '',
    consented_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at       TIMESTAMPTZ
);

CREATE TABLE user_identities (
    id               UUID PRIMARY KEY,
    user_id          UUID NOT NULL REFERENCES users(id),
    provider         TEXT NOT NULL,
    provider_subject TEXT NOT NULL,
    provider_scope   TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_subject, provider_scope)
);

CREATE TABLE provider_messages (
    id                  UUID PRIMARY KEY,
    provider            TEXT NOT NULL,
    provider_chat_id    TEXT NOT NULL,
    provider_message_id TEXT NOT NULL,
    user_id             UUID REFERENCES users(id),
    event_type          TEXT NOT NULL,
    payload_hash        TEXT NOT NULL,
    raw_payload_json    JSONB NOT NULL,
    received_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at        TIMESTAMPTZ,
    status              TEXT NOT NULL DEFAULT 'received'
                        CHECK (status IN ('received','processed','duplicate','failed')),
    UNIQUE (provider, provider_chat_id, provider_message_id)
);
CREATE INDEX idx_provider_messages_user ON provider_messages(user_id);

CREATE TABLE receipt_documents (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL REFERENCES users(id),
    provider_message_id UUID REFERENCES provider_messages(id),
    storage_key         TEXT NOT NULL DEFAULT '',
    source_url_hash     TEXT NOT NULL DEFAULT '',
    content_type        TEXT NOT NULL DEFAULT '',
    byte_size           BIGINT NOT NULL DEFAULT 0,
    sha256              TEXT NOT NULL DEFAULT '',
    perceptual_hash     TEXT NOT NULL DEFAULT '',
    status              TEXT NOT NULL DEFAULT 'received' CHECK (status IN (
        'received','queued','downloading','stored','extracting','review_required',
        'confirmed','deleted','failed_transient','failed_permanent')),
    retention_policy    TEXT NOT NULL DEFAULT 'originals_30d',
    delete_after        TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX idx_receipts_user ON receipt_documents(user_id);
CREATE INDEX idx_receipts_status ON receipt_documents(status);
CREATE INDEX idx_receipts_sha256 ON receipt_documents(user_id, sha256);

CREATE TABLE receipt_processing_attempts (
    receipt_id        UUID NOT NULL REFERENCES receipt_documents(id),
    processor_name    TEXT NOT NULL,
    processor_version TEXT NOT NULL,
    attempt_number    INT NOT NULL,
    status            TEXT NOT NULL CHECK (status IN ('started','succeeded','failed')),
    started_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at      TIMESTAMPTZ,
    error_class       TEXT NOT NULL DEFAULT '',
    error_code        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (receipt_id, processor_name, processor_version, attempt_number)
);

CREATE TABLE categories (
    id          UUID PRIMARY KEY,
    system_key  TEXT NOT NULL UNIQUE,
    display_name TEXT NOT NULL,
    parent_id   UUID REFERENCES categories(id),
    is_system   BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE merchants (
    id              UUID PRIMARY KEY,
    canonical_name  TEXT NOT NULL,
    normalised_name TEXT NOT NULL UNIQUE,
    merchant_family TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE merchant_aliases (
    id               UUID PRIMARY KEY,
    merchant_id      UUID NOT NULL REFERENCES merchants(id),
    alias            TEXT NOT NULL,
    normalised_alias TEXT NOT NULL UNIQUE,
    source           TEXT NOT NULL DEFAULT 'seed'
                     CHECK (source IN ('seed','extraction','user'))
);

CREATE TABLE transactions (
    id                  UUID PRIMARY KEY,
    user_id             UUID NOT NULL REFERENCES users(id),
    receipt_document_id UUID REFERENCES receipt_documents(id),
    account_id          UUID,
    type                TEXT NOT NULL
                        CHECK (type IN ('expense','income','refund','transfer','adjustment')),
    merchant_id         UUID REFERENCES merchants(id),
    merchant_name       TEXT NOT NULL DEFAULT '',
    description         TEXT NOT NULL DEFAULT '',
    amount_minor        BIGINT NOT NULL CHECK (amount_minor >= 0),
    currency            TEXT NOT NULL,
    occurred_at         TIMESTAMPTZ NOT NULL,
    category_id         UUID REFERENCES categories(id),
    status              TEXT NOT NULL DEFAULT 'draft' CHECK (status IN (
        'draft','awaiting_confirmation','confirmed','amended','deleted')),
    source              TEXT NOT NULL DEFAULT 'receipt'
                        CHECK (source IN ('receipt','manual')),
    confidence_summary  TEXT NOT NULL DEFAULT '',
    confirmed_at        TIMESTAMPTZ,
    version             INT NOT NULL DEFAULT 1,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at          TIMESTAMPTZ
);
CREATE INDEX idx_tx_user_occurred ON transactions(user_id, occurred_at);
CREATE INDEX idx_tx_user_status ON transactions(user_id, status);
CREATE INDEX idx_tx_receipt ON transactions(receipt_document_id);

CREATE TABLE extracted_fields (
    id                  UUID PRIMARY KEY,
    receipt_document_id UUID NOT NULL REFERENCES receipt_documents(id),
    field_name          TEXT NOT NULL,
    raw_value           TEXT NOT NULL DEFAULT '',
    normalised_value    TEXT NOT NULL DEFAULT '',
    confidence          DOUBLE PRECISION NOT NULL CHECK (confidence BETWEEN 0 AND 1),
    source              TEXT NOT NULL DEFAULT 'extractor',
    evidence_json       JSONB NOT NULL DEFAULT '{}',
    extractor_name      TEXT NOT NULL DEFAULT '',
    extractor_version   TEXT NOT NULL DEFAULT '',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_extracted_fields_receipt ON extracted_fields(receipt_document_id);

CREATE TABLE predictions (
    id                    UUID PRIMARY KEY,
    transaction_id        UUID NOT NULL REFERENCES transactions(id),
    prediction_type       TEXT NOT NULL,
    predicted_value       TEXT NOT NULL,
    confidence            DOUBLE PRECISION NOT NULL CHECK (confidence BETWEEN 0 AND 1),
    model_name            TEXT NOT NULL,
    model_version         TEXT NOT NULL,
    feature_snapshot_json JSONB NOT NULL DEFAULT '{}',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_predictions_tx ON predictions(transaction_id);

CREATE TABLE corrections (
    id              UUID PRIMARY KEY,
    transaction_id  UUID NOT NULL REFERENCES transactions(id),
    field_name      TEXT NOT NULL,
    predicted_value TEXT NOT NULL DEFAULT '',
    corrected_value TEXT NOT NULL DEFAULT '',
    source          TEXT NOT NULL DEFAULT 'chat_edit',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_corrections_tx ON corrections(transaction_id);

CREATE TABLE user_merchant_rules (
    user_id                    UUID NOT NULL REFERENCES users(id),
    merchant_id                UUID NOT NULL REFERENCES merchants(id),
    preferred_category_id      UUID REFERENCES categories(id),
    preferred_transaction_type TEXT NOT NULL DEFAULT 'expense',
    preferred_tags_json        JSONB NOT NULL DEFAULT '[]',
    sample_count               INT NOT NULL DEFAULT 0,
    confidence                 DOUBLE PRECISION NOT NULL DEFAULT 0
                               CHECK (confidence BETWEEN 0 AND 1),
    last_confirmed_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, merchant_id)
);

CREATE TABLE insights (
    id                UUID PRIMARY KEY,
    user_id           UUID NOT NULL REFERENCES users(id),
    insight_type      TEXT NOT NULL,
    period_start      TIMESTAMPTZ NOT NULL,
    period_end        TIMESTAMPTZ NOT NULL,
    payload_json      JSONB NOT NULL,
    evidence_json     JSONB NOT NULL DEFAULT '{}',
    generator_version TEXT NOT NULL,
    status            TEXT NOT NULL DEFAULT 'ready' CHECK (status IN ('ready','dismissed')),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    dismissed_at      TIMESTAMPTZ
);
CREATE INDEX idx_insights_user_period ON insights(user_id, period_start);

CREATE TABLE usage_counters (
    scope       TEXT NOT NULL CHECK (scope IN ('global','user')),
    scope_id    TEXT NOT NULL DEFAULT '',
    period      TEXT NOT NULL,
    metric      TEXT NOT NULL,
    count       BIGINT NOT NULL DEFAULT 0,
    limit_value BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, scope_id, period, metric)
);

-- Local queue (SQS replacement). Visibility timeout + attempts + dead status
-- mirror SQS semantics closely enough that an SQS adapter is drop-in later.
CREATE TABLE queue_jobs (
    id           UUID PRIMARY KEY,
    kind         TEXT NOT NULL,
    payload_json JSONB NOT NULL,
    dedupe_key   TEXT,
    status       TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','dead')),
    attempts     INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 5,
    run_after    TIMESTAMPTZ NOT NULL DEFAULT now(),
    visible_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_queue_jobs_dedupe ON queue_jobs(dedupe_key) WHERE dedupe_key IS NOT NULL;
CREATE INDEX idx_queue_jobs_poll ON queue_jobs(kind, status, run_after) WHERE status = 'queued';
CREATE INDEX idx_queue_jobs_visibility ON queue_jobs(visible_at) WHERE status = 'running';

-- Idempotent outbound outbox; the notification worker is the only sender.
CREATE TABLE outbound_messages (
    id               UUID PRIMARY KEY,
    user_id          UUID NOT NULL REFERENCES users(id),
    provider         TEXT NOT NULL,
    provider_chat_id TEXT NOT NULL,
    idempotency_key  TEXT NOT NULL UNIQUE,
    body             TEXT NOT NULL,
    status           TEXT NOT NULL DEFAULT 'queued'
                     CHECK (status IN ('queued','sent','failed','suppressed')),
    attempts         INT NOT NULL DEFAULT 0,
    last_error       TEXT NOT NULL DEFAULT '',
    sent_at          TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_outbound_status ON outbound_messages(status) WHERE status = 'queued';

-- Server-side chat state: the next text from this user resolves this action.
CREATE TABLE pending_actions (
    id             UUID PRIMARY KEY,
    user_id        UUID NOT NULL REFERENCES users(id),
    kind           TEXT NOT NULL CHECK (kind IN (
        'confirm_extraction','edit_total','edit_merchant','edit_date',
        'edit_category','edit_type','delete_account')),
    transaction_id UUID REFERENCES transactions(id),
    payload_json   JSONB NOT NULL DEFAULT '{}',
    expires_at     TIMESTAMPTZ NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX idx_pending_actions_user ON pending_actions(user_id);
