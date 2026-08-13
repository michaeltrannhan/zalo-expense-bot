-- One active manual draft per inbound provider message. Receipt drafts are
-- already unique on receipt_document_id (0010).
ALTER TABLE transactions
    ADD COLUMN IF NOT EXISTS provider_message_id UUID REFERENCES provider_messages(id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_tx_manual_provider_message
    ON transactions(provider_message_id)
    WHERE provider_message_id IS NOT NULL
      AND deleted_at IS NULL
      AND status <> 'deleted';
