-- The webhook that confirms account deletion remains only as a sanitized,
-- short-lived idempotency tombstone. This absorbs provider retries without
-- retaining its payload or recreating the deleted account.

ALTER TABLE provider_messages
    ADD COLUMN delete_after TIMESTAMPTZ;

CREATE INDEX idx_provider_messages_delete_after
    ON provider_messages(delete_after)
    WHERE delete_after IS NOT NULL;
