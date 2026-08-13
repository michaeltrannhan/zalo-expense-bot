-- One non-deleted draft transaction per receipt. Partial unique index keeps
-- soft-deleted rows from blocking a legitimate re-draft after discard.
CREATE UNIQUE INDEX idx_tx_receipt_active
    ON transactions(receipt_document_id)
    WHERE receipt_document_id IS NOT NULL
      AND deleted_at IS NULL
      AND status <> 'deleted';
