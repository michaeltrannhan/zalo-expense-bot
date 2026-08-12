-- Make webhook processing a leased claim instead of a one-shot insert.
-- A provider retry may reclaim failed work immediately or received work
-- after the handler lease expires. The receipt index keeps a reclaimed image
-- message from creating two receipt pipelines.

ALTER TABLE provider_messages
    ADD COLUMN processing_started_at TIMESTAMPTZ;

UPDATE provider_messages
SET processing_started_at = received_at
WHERE status = 'received';

CREATE UNIQUE INDEX idx_receipts_provider_message
    ON receipt_documents(provider_message_id)
    WHERE provider_message_id IS NOT NULL;
