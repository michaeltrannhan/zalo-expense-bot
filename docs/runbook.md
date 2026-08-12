# Runbook (P5-C01)

Local pilot stack. AWS deltas are marked **[post-AWS]**.

## Processes

| Process | Command | Role |
|---|---|---|
| PostgreSQL | `make up` | Only infrastructure dependency |
| Migrations | `make migrate` | Apply `db/migrations` (idempotent) |
| api | `bin/api` (or `-poll`) | Webhook ingress / long polling |
| receipt-worker | `bin/receipt-worker` | Receipt pipeline + retention sweep |
| notification-worker | `bin/notification-worker` | Outbound sends (only Zalo caller) + scheduled-summary poller |
| local playground | `make playground` | Loopback browser chat; all local adapters in one process |
| real E2E supervisor | `make e2e-real` | Credential preflight, sender pinning, isolated real Zalo/Gemini assertion |

Startup order: postgres → migrate → workers/api (any order; all retry).
Health check: `GET /healthz` (DB ping). Graceful stop: SIGINT/SIGTERM —
in-flight jobs finish, unacked jobs return via visibility timeout.

## Demo / smoke

- `make playground` — interactive local chat at `http://127.0.0.1:8090`.
  It starts PostgreSQL, migrates automatically, forces development mode,
  mock OCR, local object storage and the log provider, and needs no secrets.
  The page exposes settings and all receipt fixtures. It creates and uses the
  separate `zl_expense_playground` database so the normal local bot's users
  and queue are not touched.
- `make simulate` — full asserted loop (Gate 2), resets user data by default.
- Provider-only health check: `make provider-check` validates the token via
  Zalo `getMe` and OCRs a generated PNG with the configured Gemini model. It
  sends no Zalo message and prints no credential.
- Real bot locally: prefer `make e2e-real`; after that passes, a manual run is
  `make run-local API_FLAGS=-poll` with a non-empty `PILOT_ALLOWLIST`.

## Supervised real-provider E2E

Prerequisites: Docker, Go, a Zalo bot token, a strong webhook secret, and a
Gemini API key in the ignored `.env`. Use the current provider settings:

```env
ZALO_API_BASE=https://bot-api.zaloplatforms.com
EXTRACTOR=gemini
GEMINI_MODEL=gemini-3.6-flash
GEMINI_API_BASE=https://generativelanguage.googleapis.com
```

Run:

```sh
make provider-check  # no user message; generated receipt only
make e2e-real        # interactive full path
```

`make e2e-real` deliberately recreates only the local
`zl_expense_e2e` database and purges only the prior run's fixed `data/e2e`
object directory. Before the stack starts, pgx parses the connection string;
the effective destination must be the `zl_expense_e2e` database on a loopback
host, so URL query overrides cannot redirect the run. It first asks the
operator to send a random
one-time text; discovery ignores all other messages and does not reply. The
matched sender ID stays in a mode-0600 temporary file and becomes the sole
pilot allowlist entry for that process. Because pilot validation remains
fail-closed even in polling mode, the runner also generates an ephemeral strong
webhook secret for its local processes instead of accepting a development
placeholder. The runner then asks for `/batdau`, a clear synthetic/redacted
receipt photo, and `xác nhận`.

Success requires all of the following, checked from provider responses and
the isolated database rather than log text:

1. Zalo `getMe` accepts the configured token.
2. Gemini Flash extracts the exact known fields from a generated PNG.
3. Real Zalo polling receives onboarding and image events.
4. The media is downloaded, validated and stored locally.
5. A succeeded `gemini-vision/<configured model>` attempt and required field
   provenance are persisted.
6. A financially valid draft and review card are created and the card reaches
   Zalo.
7. The user's confirmation moves the transaction and receipt to confirmed,
   and its acknowledgement reaches Zalo.
8. No queue job is dead and no outbound row is failed or ambiguous.

The adapter accepts both the documented image fields and the `photo_url`
string observed from the live `getUpdates` endpoint; this shape is covered by
a regression test so an image event cannot silently become media-less again.

Ctrl-C stops the supervisor and all three children. The next E2E invocation
resets the dedicated database so exact-image duplicate detection does not make
reruns flaky. It also removes the prior E2E originals before their metadata is
dropped, preventing orphaned receipt files. `data/e2e` is separated from normal
local originals.

Privacy: use a synthetic or sanitized receipt unless the Gemini API key's
Cloud project has active billing and the intended users have consented. The
current [Gemini API terms](https://ai.google.dev/gemini-api/terms) say unpaid
inputs and outputs may be used for product improvement and reviewed by humans,
and explicitly advise against personal or confidential information.

Storage boundary: Zalo sends a provider-hosted image path but does not document
it as durable storage. The worker downloads the bytes, sends them to the OCR
backend, and stores its own original locally or in S3 under the current
`originals_30d` policy. The raw provider-event metadata (including the source
path) is also stored in PostgreSQL under the pilot retention policy. A
metadata-only design would need an explicit retry/reprocessing tradeoff and is
not the current implementation. Successful E2E rows and the synthetic image
copy remain locally inspectable until the next E2E reset.

Zalo documents `getUpdates` as a local-development mechanism that cannot run
at the same time as a configured webhook. Use an HTTPS webhook in production.

If PostgreSQL port 5432 is occupied, use:

```sh
POSTGRES_PORT=5433 make playground
```

The playground refuses non-loopback listeners. Create a new test user from
the page instead of deleting other local rows; each browser session gets an
isolated provider identity.

## Kill switches

| Switch | Effect | When |
|---|---|---|
| `EXTRACTION_ENABLED=false` | Receipts get "tạm tắt" notice; manual entry keeps working | OCR cost/bug incident |
| `OUTBOUND_ENABLED=false` | Outbound rows marked suppressed, nothing sent | Spam/loop incident |
| `MONTHLY_OCR_PAGE_LIMIT` | Global extraction cap/month | Cost control |
| `PER_USER_DAILY_RECEIPT_LIMIT` | Per-user image cap/day | Abuse control |
| `ZALO_MONTHLY_MESSAGE_LIMIT` | Send cap; warns 70/85/95% | Zalo Basic quota |

Changes take effect on process restart.

## Scheduled summaries

- `/tongket` shows the user's current daily/weekly/monthly settings.
- The notification worker polls due schedules every
  `SUMMARY_SCHEDULE_POLL_INTERVAL` (30 seconds by default).
- Daily summaries cover yesterday; weekly runs Monday for the completed
  week; monthly runs on the first for the completed month, all in the user's
  timezone.
- After downtime, the worker sends only the latest completed period, not a
  burst of every missed summary. Outbound idempotency prevents duplicate
  queue entries across concurrent schedulers and repairs a crash between
  the outbox insert and queue insert.
- `OUTBOUND_ENABLED=false` suppresses scheduled summaries through the same
  audited outbox path as interactive messages.

## User settings

- `/caidat` shows timezone, default currency, locale and enabled summary
  schedules. It is also linked from onboarding and `/trogiup`.
- `/caidat muigio <IANA name>` changes timezone and immediately recalculates
  every enabled schedule's next delivery. Example:
  `/caidat muigio Asia/Ho_Chi_Minh`.
- `/caidat tiente <ISO code>` changes the fallback for manual amounts without
  an explicit symbol. Example: after `/caidat tiente USD`, `50 lunch` records
  a suggested `$50.00` transaction; `$50 lunch` remains explicitly USD in
  every profile.
- Invalid timezone and currency values are rejected without changing stored
  preferences. Locale remains `vi-VN` for the Vietnamese-only MVP.

## Outbound delivery states

Zalo's supported `sendMessage` request does not carry a provider-side
idempotency key. The worker persists `sending` before the network call. If a
worker restarts in that state, or receives a transport error after the call
may have left the process, the row becomes `ambiguous` and is **not sent
again automatically**. Check the Zalo conversation/provider logs before any
manual action; resending can duplicate a message the user already received.

```sql
SELECT id, provider_chat_id, idempotency_key, attempted_at, last_error
FROM outbound_messages
WHERE status = 'ambiguous'
ORDER BY attempted_at;
```

## Quota breach

Symptoms: logs `zalo monthly message attempt threshold`, outbound rows
`suppressed` with reason. The hard provider-call cap uses
`zalo_message_attempts`; the separately reported successful-send counter is
`zalo_messages_sent`. Both are charged at most once per outbox row. Raise the
limit only after understanding the driver. Suppressed messages are not resent
automatically.

## Dead-letter inspection

```sql
SELECT kind, status, count(*) AS jobs,
       max(now() - created_at) AS oldest_age
FROM queue_jobs
WHERE status IN ('queued', 'running', 'dead')
GROUP BY kind, status
ORDER BY kind, status;

SELECT id, kind, attempts, max_attempts, last_error, updated_at
FROM queue_jobs
WHERE status = 'dead'
ORDER BY updated_at DESC;
```

Before requeueing, match the payload to its source row and confirm the
handler is idempotent. Requeue one known-safe job after fixing its cause:

```sql
UPDATE queue_jobs SET status='queued', attempts=0, run_after=now() WHERE id = '<id>';
```

Receipt rows stranded in `failed_transient` resume automatically on the
next retry; `failed_permanent` rows are inspectable via
`receipt_processing_attempts`.

Escalation thresholds for the local pilot:

- oldest queued receipt > 2 minutes: inspect the receipt worker and provider
  download/extractor errors;
- oldest queued outbound > 1 minute: inspect the notification worker, kill
  switch, quota and `ambiguous` rows;
- any dead job: operator review required; never bulk-requeue;
- running job older than its visibility deadline: verify another lane has
  reclaimed it before intervening.

## Token/secret rotation

1. Generate new bot token in Zalo Bot Manager / new webhook secret.
2. Update `.env` **[post-AWS: Secrets Manager]**, restart api + workers.
3. Invalidate the old token; watch for 401s in api logs.

## Backup / restore drill (pilot)

Back up weekly and before every migration:

```sh
docker compose exec -T postgres pg_dump -U postgres -Fc zl_expense > zl-expense.dump
```

Monthly restore drill (use an isolated database, never the live name):

```sh
docker compose exec -T postgres createdb -U postgres zl_expense_restore
docker compose exec -T postgres pg_restore -U postgres \
  -d zl_expense_restore --clean --if-exists < zl-expense.dump
docker compose exec -T postgres psql -U postgres -d zl_expense_restore \
  -c 'SELECT count(*) FROM users' \
  -c 'SELECT status, count(*) FROM queue_jobs GROUP BY status' \
  -c 'SELECT max(version) FROM schema_migrations'
docker compose exec -T postgres dropdb -U postgres zl_expense_restore
```

Record dump time, restore duration, row-count sanity checks and operator.
Receipt originals in `data/` are expendable by the 30-day policy; the
database is the system of record. **[post-AWS: RDS automated snapshots,
PITR, and the same isolated restore verification.]**

## Load smoke

Run only in `APP_ENV=development` with the log provider, extraction disabled,
and a disposable database. Send 100 unique, valid text webhooks at concurrency
10; require zero non-2xx responses, one `provider_messages` row per message,
and no dead jobs. Replay the same 100 IDs and require the row count not to
change. Capture API latency externally and verify the execution-plan pilot
gate (webhook P95 under 500 ms). Never load-test the real Zalo bot or paid OCR
endpoint.

CI (`.github/workflows/ci.yml`) gates formatting, vet, unit/integration/race
tests, build, migrations and the deterministic end-to-end simulation.

## Data rights requests

- Export: user sends `/xuatdulieu` (CSV + JSON, including summary
  preferences, written under `DATA_DIR`).
- Deletion: user sends `/xoadulieu`, confirms. Individual row: "xóa khoản
  gần nhất". Full deletion also removes scheduled-summary preferences.
  Operator-assisted deletion: `internal/account.DeleteAccount`.
