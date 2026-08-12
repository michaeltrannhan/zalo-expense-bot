DROP INDEX IF EXISTS idx_provider_messages_delete_after;

ALTER TABLE provider_messages
    DROP COLUMN IF EXISTS delete_after;
