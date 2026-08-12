-- Zalo's sendMessage request has no provider-side idempotency key in the
-- supported contract. Persist an attempt before the network call and record
-- an explicit ambiguous state after any crash/uncertain transport outcome;
-- never silently resend a message that may already have been delivered.

ALTER TABLE outbound_messages
    DROP CONSTRAINT outbound_messages_status_check;

ALTER TABLE outbound_messages
    ADD CONSTRAINT outbound_messages_status_check
    CHECK (status IN ('queued','sending','sent','failed','suppressed','ambiguous')),
    ADD COLUMN attempted_at TIMESTAMPTZ,
    ADD COLUMN provider_message_id TEXT NOT NULL DEFAULT '';
