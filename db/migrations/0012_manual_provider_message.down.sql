DROP INDEX IF EXISTS idx_tx_manual_provider_message;
ALTER TABLE transactions DROP COLUMN IF EXISTS provider_message_id;
