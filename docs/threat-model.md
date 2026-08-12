# Threat model — Zalo expense tracker (P5-A01)

Scope: receipt-first Zalo Bot, pilot audience of relatives. Data at stake:
receipt images (may contain purchases, partial addresses), spending history,
Zalo sender IDs. No banking data, no credentials beyond the bot token and
webhook secret.

## Assets and trust boundaries

- **Inbound edge** — Zalo webhook (`POST /webhook/zalo`) or getUpdates long
  polling. Everything inbound is untrusted.
- **Object store** — receipt originals (local dir now, S3 later, SSE at
  rest, blocked public access).
- **Database** — PostgreSQL, single writer of record.
- **Outbound edge** — notification worker is the only process calling the
  Zalo send API.

## Threats and mitigations (plan §17)

| Threat | Mitigation | Status |
|---|---|---|
| Spoofed sender (someone messages as a relative) | Identity = Zalo-issued sender ID, never display name; pilot allowlist (`PILOT_ALLOWLIST`); account suspension state | Implemented |
| Forged webhook | Constant-time `X-Bot-Api-Secret-Token` verification; HTTPS-only in deployment; secret rotation runbook | Implemented |
| Duplicate processing (retries, replays) | Leased `provider_messages` claim with failed/stale recovery; one receipt per provider message; image sha256 hard-duplicate guard; soft-duplicate warning (never auto-delete); deterministic keys on outbound messages and queue jobs | Implemented |
| Oversized/malformed payloads | 1 MiB webhook body cap; strict-then-tolerant parsing; 10 MiB media cap; content sniffing (JPEG/PNG/WEBP only) | Implemented |
| Prompt-injection via receipt text | Extraction output is data, never executed; LLM (future) may phrase insights but never calculates numbers — SQL + deterministic logic only; Vietnamese template rendering, no user-controlled format strings | Implemented (design rule) |
| Data theft | Object store private + SSE; originals deleted after 30 days (retention sweep); raw webhook payloads retention-limited; logs redact identifiers (hashed user/chat IDs, no receipt content); `/xuatdulieu` self-export; `/xoadulieu` lock-coordinated physical purge with retryable object deletion | Implemented locally; S3 bucket policy pending AWS deploy |
| Bot token compromise | Token only in env, never in code/logs/terraform; rotate via Bot Manager (runbook); kill switches limit blast radius | Runbook |
| Quota denial-of-service (message/image floods) | Per-user daily receipt limit; global monthly OCR page cap; Zalo monthly message cap with 70/85/95% warnings; suppression (not failure) on quota breach | Implemented |
| Silent financial corruption | Confidence flags; refund/transfer never silently accepted; totals always user-confirmed; optimistic version on transactions; corrections are first-class audit data | Implemented |
| Insider data access | Single-operator pilot; DB access via local psql only; no admin web surface | Accepted (pilot) |

## Out of scope (documented)

- Row-level security in PostgreSQL: deferred (single-tenant-per-row access
  is enforced at the store layer; RLS lands with the multi-user Mini App).
- Rate limiting per IP at the edge: Zalo is the only webhook caller;
  deployment adds an ALB/API Gateway rule in Phase 5.
