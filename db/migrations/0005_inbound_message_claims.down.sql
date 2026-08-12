DROP INDEX IF EXISTS idx_receipts_provider_message;

ALTER TABLE provider_messages
    DROP COLUMN IF EXISTS processing_started_at;
