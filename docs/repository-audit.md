# Repository audit

Reviewed against `docs/execution-plan.md` on 2026-07-23. The critical local
correctness/privacy work is tracked with evidence in
`docs/critical-fix-checkpoints.md`.

## Critical findings resolved

- The Go bot was an untracked child of an unrelated repository.
- Failed or crashed inbound webhooks could never be retried.
- Account deletion left provider payloads, derived financial records, message
  copies, jobs, usage, rules, insights and CSV/JSON exports behind.
- In-flight workers could recreate user data after deletion.
- Provider retries could recreate an account immediately after deletion.
- Outbound retries could double-send while quota counted attempts as success.
- Invalid configuration silently fell back; pilot access could start open.
- There was no CI gate or actionable restore/backlog procedure.

## Remaining high-priority gaps

### Production infrastructure is not implemented

The execution plan calls for AWS deployment, private S3, RDS, SQS/DLQ,
secrets management and infrastructure as code. The repository has S3 and
Textract adapters, but runtime queueing remains PostgreSQL-only and there is
no Terraform/deployment stack. The app is local-pilot ready, not
production-deployable.

### Required metrics and alerts are absent

The API exposes a database health check and processes emit structured logs,
but the execution-plan metrics (webhook latency, queue age, processing
latency, OCR failures, quota utilization, ambiguous sends, dead letters) are
not exported to Prometheus/CloudWatch and have no automated alerts. The
runbook queries are manual safeguards.

### Media download needs a provider-host allowlist

The Zalo adapter caps bytes/time/redirects, but accepts any `http(s)` media
URL delivered by the authenticated provider. Before a public production
webhook, restrict initial and redirected destinations to verified Zalo media
hosts and block loopback, link-local and private IP ranges. This needs current
official Zalo host documentation rather than a guessed allowlist.

## Remaining medium-priority gaps

- Raw inbound payloads and non-ephemeral outbound copies have no automatic
  90-day retention sweep; they are removed on account deletion only.
- `/xuatdulieu` writes local paths rather than securely delivering files in
  chat. Artifacts are mode `0600` and now removed on account deletion, but
  should also have a short independent expiry.
- Backup/restore and load checks are documented but not automated as
  scheduled drills with recorded evidence.
- `queue_jobs` and terminal outbox rows have no general archival/pruning job,
  so long-running pilots will accumulate operational history.
- Production row-level security remains deferred; authorization is enforced
  in application queries and is appropriate only for the current pilot.

## Suggested next order

1. Add provider-host/IP validation for media downloads.
2. Add metrics, dashboards and alerts for queue age, failures and ambiguous
   outbound delivery.
3. Add bounded retention for raw payloads, exports, terminal jobs and outbox.
4. Implement Terraform plus SQS/DLQ/RDS/S3/Secrets Manager.
5. Run and record restore and load gates before inviting pilot users.
