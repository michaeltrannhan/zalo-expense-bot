# Critical-fix checkpoints

This ledger mirrors the active `/goal` for making `zl-expese-bot` a
self-contained, pilot-safe repository. A checkpoint is complete only after
its implementation and verification evidence are recorded here.

## CP-1 — Repository boundary and project material

**Status:** complete

- The Go bot has its own Git repository boundary.
- The execution plan and license live inside the bot repository.
- Local secrets and generated runtime data remain ignored.
- Unrelated sibling projects and the legacy parent application are not copied.

**Evidence:** `git rev-parse --show-toplevel` resolves to this directory;
`git check-ignore .env data/ bin/` confirms local-only files stay untracked.

## CP-2 — Retryable inbound webhook processing

**Status:** complete

- A provider retry can reclaim `received` or `failed` messages.
- Only one worker/handler owns a message attempt at a time.
- The resolved internal user ID is persisted on the provider message.
- Tests cover route failure, retry, duplicate completion, and stale claims.

**Evidence:** migration `0005_inbound_message_claims` adds leased attempts and
one receipt per provider message; store integration tests cover active,
failed, stale, completed, and payload-conflict claims; the bot integration
test injects a queue failure and proves the retry produces one owned provider
message, receipt, usage increment, receipt job, and outbound reply.

## CP-3 — Complete, race-safe account deletion

**Status:** complete

- Account deletion removes or anonymizes every user-linked data class listed
  in the privacy map, including raw provider payloads and asynchronous jobs;
  only a sanitized 24-hour retry tombstone remains for the triggering key.
- In-flight receipt/outbound work cannot recreate data after deletion.
- Tests prove no user-linked operational or analytics row remains.

**Evidence:** account deletion now shares a per-user advisory lock with
inbound, receipt, outbound, and scheduled-summary work; it purges queue jobs,
provider payloads, all receipt/transaction derivatives, insights, rules,
usage, message copies and local CSV/JSON exports. The triggering webhook is
reduced to a 24-hour payload-free retry tombstone so a concurrent provider
retry cannot recreate the account. Integration tests verify the full data graph,
object-store failure rollback/retry, lock waiting, unrelated-data isolation,
and one terminally purged deletion-confirmation message.

## CP-4 — Outbound delivery and quota correctness

**Status:** complete

- Quota counts successful sends once, not delivery attempts.
- Provider idempotency is used where supported; ambiguous delivery failures
  are explicit and do not silently double-send.
- Tests cover provider failures and the send/commit crash boundary.

**Evidence:** migration `0007_outbound_delivery_state` adds `sending` and
`ambiguous` states plus provider receipts. A provider-call attempt is
reserved atomically before sending; successful quota is charged atomically
with `sent`. Integration tests prove successful replay, transient ambiguity,
crash recovery, quota suppression before the provider call, and ephemeral
deletion-receipt cleanup.

## CP-5 — Fail-closed configuration

**Status:** complete

- Invalid booleans, integers, durations, and enum values stop startup.
- Production/pilot mode cannot start with an empty allowlist.
- Risky extraction and outbound defaults are explicit by environment.
- Configuration validation has focused tests.

**Evidence:** `APP_ENV` selects explicit development/test/pilot/production
safety rules; all scalar parsers return errors on malformed values; external
extraction/outbound work defaults off; ranges, enums, addresses and secure
URLs are validated. Pilot/production refuse empty allowlists, missing tokens,
placeholder secrets and unlimited quotas. Focused unit tests cover defaults,
bad scalar values, pilot gates and production backend requirements.

## CP-6 — Delivery safeguards and final verification

**Status:** complete

- CI runs formatting, vet, unit tests, integration tests, and build checks.
- Operational runbooks cover restore, load, queue backlog, and failed jobs.
- The full local verification ladder passes and remaining non-critical flaws
  are documented with severity and suggested order.

**Evidence:** `.github/workflows/ci.yml` gates formatting, vet, unit,
integration, race, build, migrations and simulation. The runbook includes
queue-age/dead-letter thresholds, ambiguous-send handling, an isolated
restore drill and a bounded load-smoke protocol. `docs/repository-audit.md`
ranks the remaining production gaps, and the local supervisor tears down the
stack if any child process exits.

## CP-7 — Visible settings and local browser testing

**Status:** complete

- `/caidat` exposes timezone, default currency, locale and summary schedules.
- Timezone and currency updates are validated and persisted; enabled summary
  schedules are recalculated after timezone changes.
- Manual entries without a currency symbol use the user's configured default.
- `make playground` supplies a loopback-only browser chat using the real bot
  handler, isolated PostgreSQL queue paths, local object storage and embedded
  mock OCR.
- The playground makes settings and receipt fixtures visible without Zalo or
  cloud credentials and cannot bind to a non-loopback address.

**Evidence:** `go test ./...`, `make test-integration`, `make race`,
`make simulate`, `make lint`, and `go build ./...` pass. Browser smoke at
1280×800 and an iPhone-sized viewport verifies onboarding, `/caidat`, USD
preference persistence, `50 lunch` rendering as `50,00 $`, and the Co.opmart
fixture's acknowledgement + extraction card; the browser reports no console
or failed-network errors. The playground starts against the isolated
`zl_expense_playground` database and rejects non-loopback listeners in tests.

## CP-8 — Real Zalo and Gemini Flash E2E

**Status:** complete

- Provider defaults and parsers match the current official Zalo API while
  preserving legacy fixture compatibility, including the `photo_url` string
  observed in a real `getUpdates` image event.
- A secret-safe preflight validates the actual bot token and Gemini key/model
  with read-only metadata plus a generated OCR fixture.
- A supervised runner uses a dedicated database and local object directory,
  validates the effective pgx destination, deletes prior E2E objects before
  their metadata, discovers and allowlists exactly one sender, and checks
  persisted state rather than trusting console text.
- The full proof must include real Zalo onboarding, image download, Gemini
  extraction/provenance, review-card delivery and user confirmation.
- Processing-attempt state, tests, operator docs and an independent
  `$review-agent` pass must all agree before this checkpoint is complete.

**Evidence:** live `getMe` succeeds for the supplied token;
`gemini-3.6-flash` is available to the supplied key and extracts every asserted
field from the generated PNG; unit tests cover the current Zalo wrapper,
single-photo field, millisecond timestamps, POST polling, message constraints,
token redaction and Gemini request ordering. Unit, integration, race, lint,
build and shell checks pass. The independent review's six actionable findings
were fixed and its final pass reports no findings. The supervised real-message
run passed onboarding, the live `photo_url` shape, image download, Gemini OCR,
persisted provenance, review-card delivery and user confirmation; database
audit found one confirmed receipt and transaction with no bad outbound or dead
job.
