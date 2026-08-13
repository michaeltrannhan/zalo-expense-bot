-- Claim tokens make Ack/Nack/Heartbeat ownership-safe: a stale worker whose
-- lease expired cannot complete another worker's claim.
ALTER TABLE queue_jobs
    ADD COLUMN claim_token UUID;

CREATE INDEX idx_queue_jobs_claim ON queue_jobs(id, claim_token)
    WHERE status = 'running' AND claim_token IS NOT NULL;
