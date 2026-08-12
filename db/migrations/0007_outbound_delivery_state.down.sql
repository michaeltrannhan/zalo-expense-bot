ALTER TABLE outbound_messages
    DROP COLUMN IF EXISTS provider_message_id,
    DROP COLUMN IF EXISTS attempted_at,
    DROP CONSTRAINT outbound_messages_status_check;

UPDATE outbound_messages
SET status = 'failed',
    last_error = CASE
        WHEN last_error = '' THEN 'downgraded from ' || status
        ELSE last_error
    END
WHERE status IN ('sending', 'ambiguous');

ALTER TABLE outbound_messages
    ADD CONSTRAINT outbound_messages_status_check
    CHECK (status IN ('queued','sent','failed','suppressed'));
