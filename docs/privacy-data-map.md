# Privacy data map (F0-04)

Plain-language version of this map is shown to users via the privacy
command ("chính sách").

## Data inventory

| Data | Where | Purpose | Retention |
|---|---|---|---|
| Zalo sender/chat IDs | `user_identities`, `provider_messages` | Authenticate the sender; idempotent webhooks | Until account deletion; the triggering message key survives sanitized for at most 24h to absorb provider retries |
| Receipt originals | Object store `receipts/{user}/{receipt}` | OCR + user reference | **30 days** (`delete_after`), hourly retention sweep |
| Raw webhook payloads | `provider_messages.raw_payload_json` | Idempotency anchor + media recovery | Until account deletion **[review: cap to 90 days post-pilot]** |
| Extracted fields | `extracted_fields` | Provenance per field (raw + normalised + confidence + extractor) | Until account deletion |
| Transactions | `transactions` | The product | Until user deletes (soft-delete) / account deletion |
| Corrections & predictions | `corrections`, `predictions` | Learning + audit. Reachable through a user-owned transaction | Until account deletion |
| Merchant rules | `user_merchant_rules` | Personal categorisation | Until account deletion |
| Insights | `insights` | Evidence-backed period summaries | Until account deletion |
| Summary preferences | `scheduled_summary_preferences` | User-selected cadence, delivery time and destination chat | Until account deletion |
| Usage counters | `usage_counters` | Quota enforcement | Rolling periods |
| Outbound copies | `outbound_messages.body` | Idempotent sends | Until account deletion **[review: cap to 90 days post-pilot]** |
| User exports | `DATA_DIR/exports/{user}` | User-requested CSV/JSON portability artifact | Until account deletion; operator should deliver and remove sooner |

## Retention classes

1. **originals_30d** — receipt images; deleted by the retention sweep
   (object removed, receipt row marked `deleted`; extracted data survives).
2. **account** — everything user-linked; physically removed by `/xoadulieu`.
   Receipt objects are deleted first; a failure leaves the database intact so
   deletion can be retried. The remaining user row is a de-identified
   lifecycle tombstone only.
3. **ephemeral deletion receipt** — one minimal outbound confirmation may
   exist after the purge and is physically removed with its queue job as soon
   as the delivery attempt succeeds or permanently fails.
4. **deletion webhook tombstone** — the triggering provider chat/message key
   only; raw payload, hash, event content and user link are cleared. The
   hourly retention sweep removes it after 24 hours.

## Flows

- **Consent**: pending users can only consent or read the privacy text;
  nothing else is processed or stored beyond the identity row.
- **Export** (`/xuatdulieu`): CSV of all non-deleted transactions + JSON
  metadata (user, consent, identities, summary preferences, counts).
- **Deletion** (`/xoadulieu`): full account deletion report; individual
  transaction deletion via "xóa khoản gần nhất". Full deletion serializes
  against inbound handlers, receipt workers, notification workers and
  scheduled summaries; it purges jobs, message copies, provider payloads,
  derived records, local CSV/JSON exports and preferences before unlinking
  the identity.
- **Logging**: no receipt content, no full sender IDs, no tokens — only
  hashed identifiers (see `internal/logging`).

## Access

Single operator (pilot). Database reachable only on localhost; object
store is a local directory **[post-AWS: private S3, SSE, blocked public
access, no presigned URLs in MVP]**.

## Third-party processing

- **Zalo Bot API** — message transport; images are fetched from
  provider-hosted URLs.
- **Google Gemini API** (only when `EXTRACTOR=gemini`) — receipt images
  are submitted for OCR. The **free tier may use submitted content for
  product improvement**; pilot users must be told, and real family
  receipts should move to a paid key (no training use) or a local model.
  **Amazon Textract** (when `EXTRACTOR=textract`) does not use content
  for training per AWS service terms.
