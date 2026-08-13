-- Revert deleting status. In-flight deletions are forced to active so the
-- narrower CHECK can be restored; operators should finish or retry deletion
-- after rolling forward again.
UPDATE users SET status = 'active', updated_at = now()
 WHERE status = 'deleting' AND deleted_at IS NULL;

ALTER TABLE users DROP CONSTRAINT IF EXISTS users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('pending','active','suspended','deleted'));
