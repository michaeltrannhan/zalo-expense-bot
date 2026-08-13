-- Allow users.status = 'deleting' for the durable account-deletion saga.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_status_check;
ALTER TABLE users ADD CONSTRAINT users_status_check
    CHECK (status IN ('pending','active','suspended','deleting','deleted'));
