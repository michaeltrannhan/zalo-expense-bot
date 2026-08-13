DROP INDEX IF EXISTS idx_queue_jobs_claim;
ALTER TABLE queue_jobs DROP COLUMN IF EXISTS claim_token;
