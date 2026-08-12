# zl-expese-bot — Zalo expense tracker

Receipt-first personal expense tracker over a Zalo Bot. A user sends a
receipt photo, the bot extracts merchant/total/date/currency, shows a
confirmation card, learns the user's category preferences from corrections,
and answers daily/weekly/monthly recorded-spending summaries — entirely in
Vietnamese chat.

Implementation follows [`docs/execution-plan.md`](docs/execution-plan.md).
Critical hardening work is tracked in
[`docs/critical-fix-checkpoints.md`](docs/critical-fix-checkpoints.md).
Remaining deployment and operational gaps are ranked in
[`docs/repository-audit.md`](docs/repository-audit.md).
Current milestone: **Phase 4 feature-complete, including a discoverable
settings surface, opt-in scheduled summaries, and a browser local lab**;
the default stack stays fully local
(PostgreSQL + mock extractor + filesystem store), so everything is
verifiable without AWS. Real OCR is one env flip away
(`EXTRACTOR=gemini` for Gemini Flash vision, or `EXTRACTOR=textract`).

## Architecture

Three long-lived processes share one PostgreSQL database; a
Postgres-backed queue (SQS semantics: visibility timeout, backoff,
dead-letter) connects them:

```
Zalo Bot API
   │ webhook (or -poll long polling)
   ▼
api ──────────────────► queue_jobs ──► receipt-worker
   bot handler: dedupe, identity,        download → validate → store →
   consent, commands, pending chat       duplicate check → mock extract →
   state; enqueues everything slow       draft tx → confirmation card
                                             │
   notification-worker ◄── outbound_messages ◄┘
   the only process that sends to Zalo; also
   schedules opt-in summaries; quota + kill switch
```

- `contracts/events` — versioned cross-process payloads (schema v1 frozen).
- `internal/messaging` — provider contract; `zalo` adapter for the real Bot
  API, `logprovider` for local runs (never dials out, prints messages).
- `internal/extraction/mock` — deterministic extractor over an embedded
  synthetic receipt corpus; `internal/extraction/textract` — real OCR via
  AnalyzeExpense behind the same contract (`EXTRACTOR=textract`).
- `internal/platform/objectstore` — receipt originals; local filesystem by
  default, private S3 with SSE behind the same contract (`OBJECTSTORE=s3`).
- `internal/receipt`, `internal/bot`, `internal/notify` — pipeline, chat
  orchestration, outbound outbox (idempotent enqueue).
- Receipt originals carry 30-day retention metadata; an hourly sweep in the
  receipt-worker deletes expired originals (extracted data survives).
- `docs/` — execution plan, critical-fix checkpoints, threat model, runbook,
  and privacy data map.

## Quick start

```sh
make playground             # then open http://127.0.0.1:8090
```

This starts PostgreSQL, applies migrations, and runs a loopback-only browser
chat with mock OCR. It needs Docker and Go, but no `.env`, Zalo token, or
cloud key. Use `POSTGRES_PORT=5433 make playground` if port 5432 is occupied.

## Local testing ladder (before AWS)

**Level 0 — interactive browser playground, no chat platform at all.**

```sh
make playground
```

Open `http://127.0.0.1:8090`. The page exposes the user's timezone,
default currency and automatic-summary schedule; it can send normal chat,
change settings, and feed every embedded receipt fixture through the real
handler, PostgreSQL queue, receipt processor and notification sender. It is
forced to loopback + local filesystem + mock extraction and never calls
Zalo, Gemini, Textract or S3. Its `zl_expense_playground` database is isolated
from the normal local bot database, so experiments cannot consume or mutate
the normal worker queue.

**Level 0b — deterministic asserted demo.**

```sh
make simulate               # full loop with assertions, resets demo data
```

**Level 1 — real processes, fake chat via curl.** One terminal:

```sh
make run-local ZALO_BOT_TOKEN= EXTRACTOR=mock \
  EXTRACTION_ENABLED=true OUTBOUND_ENABLED=true
                            # builds, migrates, runs all 3 processes
```

Then in another terminal:

```sh
curl -X POST http://127.0.0.1:8080/webhook/zalo \
  -H 'Content-Type: application/json' \
  -d '{"provider":"zalo_bot","provider_user_id":"me","provider_chat_id":"chat-1",
       "provider_message_id":"m-0001","event_type":"message.text.received",
       "text":"/batdau","received_at":"2026-07-21T10:00:00Z"}'

curl -X POST http://127.0.0.1:8080/webhook/zalo \
  -H 'Content-Type: application/json' \
  -d '{"provider":"zalo_bot","provider_user_id":"me","provider_chat_id":"chat-1",
       "provider_message_id":"m-0002","event_type":"message.image.received",
       "received_at":"2026-07-21T10:01:00Z",
       "media":[{"provider":"zalo_bot","url":"MOCK-FIXTURE:coopmart-clean","mime_type":"image/jpeg"}]}'
```

Bot replies print in the `run-local` terminal (`[BOT → …]`). Available
fixture images: `coopmart-clean`, `highlands-clean`, `petrolimex-clean`,
`guardian-low-total` (⚠️ flagged total), `shopee-refund`,
`vietcombank-transfer`, `woolworths-aud`, `coffeehouse-multi-total`,
`grab-mid`, `not-a-receipt` — then try `xác nhận`, `/homnay`, `sửa số tiền`.

**Level 2 — real-provider preflight, no user message or receipt.** Create a
bot in Zalo Bot Manager and place the credentials in the ignored `.env`:

```env
ZALO_BOT_TOKEN=<your bot token>
ZALO_WEBHOOK_SECRET=<at least 16 random characters>
ZALO_API_BASE=https://bot-api.zaloplatforms.com
GEMINI_API_KEY=<key from Google AI Studio>
GEMINI_MODEL=gemini-3.6-flash
EXTRACTOR=gemini
EXTRACTION_ENABLED=true
OUTBOUND_ENABLED=true
```

```sh
make provider-check
```

This calls Zalo `getMe` (read-only) and sends a generated, privacy-safe PNG
to Gemini. It fails unless the configured Flash model reads the known total,
currency, merchant and date. Credential values are never printed.

**Level 3 — supervised real Zalo → Gemini OCR → confirmation E2E.**

```sh
make e2e-real
```

The target recreates only the dedicated `zl_expense_e2e` database, purges only
the fixed `data/e2e` object directory from the prior run, performs
the provider preflight, and prints a one-time challenge. Send that challenge
to the bot; the runner discovers the sender without replying or printing the
ID, then starts the normal API and both workers in `APP_ENV=pilot` with the
allowlist pinned to that sender. Follow the prompts to send `/batdau`, one
clear synthetic or sanitized receipt photo, and `xác nhận`. The monitor
asserts real ingress, media download, Gemini provenance, required extracted
fields, a valid draft, successful Zalo review-card delivery, and final
confirmation. Ctrl-C stops every child process; the isolated database remains
available for inspection and both its rows and receipt objects are reset on the
next run. The runner asks pgx to parse the database URL and refuses any
effective host/database other than loopback `zl_expense_e2e`, including query
parameter overrides.

If you need a non-personal image for the phone step, generate the exact
deterministic fixture used by the provider check without overwriting files:

```sh
go run ./cmd/e2e fixture -out /tmp/zl-expense-test-receipt.png
```

Do not send personal or confidential receipts through an unpaid Gemini API
project. Under Google's current terms, unpaid inputs/outputs may be used to
improve products and may be reviewed; a project with active Cloud Billing is
treated as a paid service with different data-use terms. See the
[Gemini API terms](https://ai.google.dev/gemini-api/terms).

Zalo supplies an image path in the inbound event; its documentation does not
promise that path is durable. This application therefore downloads the image
for OCR and stores its own original under `DATA_DIR` (or S3) for the current
`originals_30d` retention window. PostgreSQL also retains the provider-event
metadata, including that source path, until account deletion under the current
pilot policy. The E2E copy is isolated under `data/e2e` and is purged with its
database at the beginning of the next E2E run. See
[`docs/privacy-data-map.md`](docs/privacy-data-map.md) before changing this
policy to metadata-only processing.

For an unsupervised manual run after the E2E passes, set a non-empty
`PILOT_ALLOWLIST` and use `make run-local API_FLAGS=-poll`. Zalo documents
long polling for local development only; production should use an HTTPS
webhook.

Ctrl-C stops all three processes cleanly. Logs are JSON on stdout;
`GET /healthz` pings the database.

## Make targets

| Target | Purpose |
|---|---|
| `make up` / `make down` | start/stop PostgreSQL |
| `make migrate` | apply migrations |
| `make playground` | interactive local browser chat/settings/receipt lab |
| `make simulate` | deterministic end-to-end demo with assertions (`-keep` keeps data) |
| `make provider-check` | read-only Zalo token check + synthetic Gemini Flash OCR assertion |
| `make e2e-real` | isolated, allowlist-pinned, supervised real-provider E2E |
| `make build` | all binaries into `bin/` |
| `make test` | unit tests |
| `make test-integration` | tests against real PostgreSQL |
| `make race` | race-enabled integration run |
| `make lint` | gofmt + go vet |

## Cost controls and kill switches (env)

- `APP_ENV=development|test|pilot|production` — selects startup safety rules;
  pilot/production refuse an empty allowlist, missing token, placeholder
  secret, unlimited quotas, or insecure provider endpoints.
- `EXTRACTION_ENABLED=false` — OCR kill switch; receipts get a polite
  "tạm tắt" reply and manual entry still works.
- `OUTBOUND_ENABLED=false` — suppress all outbound sends.
- `MONTHLY_OCR_PAGE_LIMIT` (80) — global monthly extraction cap.
- `PER_USER_DAILY_RECEIPT_LIMIT` (20) — per-user daily image cap.
- `ZALO_MONTHLY_MESSAGE_LIMIT` (3000) — hard cap on provider send attempts;
  warns at 70/85/95% and suppresses before an excess call. Successful sends
  are counted separately.
- `PILOT_ALLOWLIST` — comma-separated Zalo sender IDs allowed in (empty =
  open, development only).
- `OBJECTSTORE=local|s3` + `S3_BUCKET`/`S3_PREFIX` — receipt object backend.
- `EXTRACTOR=mock|textract|gemini` — OCR backend. `gemini` is a vision LLM
  via Google AI Studio (`GEMINI_API_KEY`, current model default
  `gemini-3.6-flash`). The model only reads raw field strings; amounts/dates are
  re-parsed deterministically and the confirmation card still gates every
  transaction. Do not submit sensitive images to an unpaid service; use a
  billing-enabled project or a local model for real family receipts.

## Feature notes

- Settings: `/caidat` shows timezone, default currency, language and all
  automatic-summary schedules in one reply. Use
  `/caidat muigio Asia/Ho_Chi_Minh` or `/caidat tiente USD` to change a
  preference. Timezone changes recalculate enabled delivery schedules;
  currency changes apply to symbol-less manual entries such as `50 lunch`.

- Period summaries: `/homnay` `/tuan` `/thang` `/tuantruoc` `/thangtruoc`,
  each persisted as an evidence-backed insight row (regeneration upserts).
  Mixed currencies remain separate through totals, categories, merchants and
  "largest transaction" rendering; raw minor units are never compared across
  currencies.
- Scheduled summaries: `/tongket ngay 20:00`, `/tongket tuan 08:00`, or
  `/tongket thang 09:00`; `/tongket` shows settings and
  `/tongket tat ca` opts out. Delivery uses the user's timezone and reports
  only the most recently completed day/week/month after worker downtime.
- Outbound safety: producer retries create one outbox intent. Because Zalo
  does not expose provider-side request idempotency, interrupted or uncertain
  sends become `ambiguous` and are never silently sent twice; successful-send
  quota is committed atomically with the provider receipt.
- Corrections: "sửa số tiền/cửa hàng/ngày/danh mục/loại", "đổi khoản gần
  nhất sang …"; every correction trains the user's merchant rule.
- Duplicates: exact-image hash guard (hard stop) + same amount/merchant/
  ±3-day soft warning (never auto-deletes).
- Deletion: "xóa khoản gần nhất" (two-step, shows the exact row),
  `/xoadulieu` (full account), `/xuatdulieu` (CSV+JSON export).
- Evaluation: `go test ./internal/extraction/mock/` scores the extractor
  per-field against `test/fixtures/receipts/ground_truth.json` at Gate-3
  accuracy floors.

## Verification status

- `make simulate` passes (10 steps, 30+ assertions): consent → image →
  card → confirm → `/homnay` → duplicate-webhook replay absorbed →
  duplicate-image guard → stale confirm → two-step individual deletion.
- Unit + integration suites pass (`make test`, `make test-integration`),
  including scheduled delivery/idempotency, retention sweep, insight
  persistence, soft-duplicate query, Textract/S3 adapters (fake clients, no
  AWS calls). Integration setup is database-serialized and restores global
  seeds, so parallel Go packages cannot corrupt one another's fixtures.
- Three-process live loop verified locally over the real queue.
- Live provider preflight verifies the configured Zalo token and proves
  `gemini-3.6-flash` OCR against a generated receipt without exposing secrets.
- Unit coverage includes settings parsing/validation and the embedded local
  playground; integration coverage verifies profile persistence, timezone
  rescheduling, and default-currency manual entries.

Next milestones: complete the in-repository critical-fix checkpoints, then
AWS deploy + real-OCR evaluation on a larger corpus (Phase 3 gate), followed
by pilot load and restoration drills (Phase 5).
