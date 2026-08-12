-- Optional scheduled summaries (P4-B03). Delivery minutes are stored as a
-- local wall-clock minute (0..1439); next_delivery_at is the UTC instant
-- derived from the user's IANA timezone.
CREATE TABLE scheduled_summary_preferences (
    id                 UUID PRIMARY KEY,
    user_id            UUID NOT NULL REFERENCES users(id),
    frequency          TEXT NOT NULL CHECK (frequency IN ('daily','weekly','monthly')),
    delivery_minute    SMALLINT NOT NULL CHECK (delivery_minute BETWEEN 0 AND 1439),
    provider           TEXT NOT NULL,
    provider_chat_id   TEXT NOT NULL,
    enabled            BOOLEAN NOT NULL DEFAULT true,
    next_delivery_at   TIMESTAMPTZ NOT NULL,
    last_delivered_at  TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, frequency)
);

CREATE INDEX idx_scheduled_summaries_due
    ON scheduled_summary_preferences(next_delivery_at)
    WHERE enabled;
