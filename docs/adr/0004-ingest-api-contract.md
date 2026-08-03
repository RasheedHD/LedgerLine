# ADR-0004: The POST /events contract

- **Status:** Accepted
- **Date:** 2026-08-03
- **Deciders:** Rasheed
- **Related:** [ADR-0001](0001-event-schema.md), [ADR-0002](0002-dedup-enforcement.md)
- **Resolves:** PLAN.md debt items D2, D4, D5, D11, D13

## Context

ADR-0001 and ADR-0002 gave incompatible instructions for the same response.

ADR-0001, Consequences:

> Rejections (duplicate, too-old) must be distinguishable in the API response.
> "Accepted", "duplicate — already have it", and "too old, use backfill" are
> three different outcomes and a client needs to tell them apart.

ADR-0002, Consequences:

> Once duplicates return `202`, callers cannot distinguish "stored" from
> "already had it." That is intentional.

Both were Accepted. Implementing the duplicate fix without settling this meant
picking one of two specifications at random, so this ADR settles it.

## Decision

**The status code carries the outcome class. The response body carries the
detail.** That satisfies both requirements at once, because they were never
actually in conflict — they were arguing about different layers.

### Success

Both a new event and a duplicate return **`202 Accepted`**:

```json
{ "id": 41, "duplicate": false }
```

The status is identical because the *outcome* is identical: exactly one event
is stored, and the client should stop retrying. `duplicate` tells a client that
cares which path it took — useful for metrics and for a client auditing its own
retry behaviour — without misleading one that only checks the status.

**Why not `409 Conflict` for a duplicate.** A duplicate is not a client error.
The canonical case is a client whose connection dropped before the original
`202` arrived: it did exactly the right thing by retrying with the same key.
`409` tells it the request was invalid, and a well-built client responds to
that by escalating or retrying harder — the opposite of what should happen.

**Where this deviates from Stripe.** Stripe replays the *original response body*
byte-for-byte. That requires storing the response alongside the key, which we
do not do yet (D14). Our bodies differ by one boolean. The distinction that
matters is that the *effect* is identical and the id is identical; only
metadata about how the request was served differs.

### Rejections

| Status | Code | When |
|---|---|---|
| `400` | `malformed_request` | Body is not valid JSON, or contains unknown fields |
| `400` | `invalid_field` | A field is missing or fails a rule |
| `413` | `malformed_request` | Body exceeds 64 KiB |
| `422` | `event_too_old` | `occurred_at` is beyond the 35-day backfill window |
| `500` | `internal_error` | Anything else |

`event_too_old` gets its own status and code rather than folding into
`invalid_field`. This is the concrete form of ADR-0001's three-outcome
requirement: "your request was malformed" and "this usage is too old to bill
automatically, use the backfill path" demand completely different client
behaviour, and a client that cannot tell them apart will retry the one it
should escalate.

Codes are stable and machine-readable; `detail` strings are for humans and may
change without notice.

### Validation rules

Applied before any database work, so a bad request never becomes a Postgres
error message that names no field.

- **`tenant_id`, `meter`, `idempotency_key`** — required, at most 255 bytes, no
  leading or trailing whitespace. The whitespace rule exists because `"acme"`
  and `"acme "` would otherwise be two tenants that look identical in every log
  and report.
- **`quantity`** — required; a plain decimal string. No scientific notation
  (accepting `1e5` invites clients to send a float's string form, reopening the
  precision hole ADR-0001 §3 closed). No negatives: a credit is a ledger
  adjustment with its own audit trail, not usage with a minus sign. At most 29
  integer and 9 fractional digits, matching `NUMERIC(38,9)` — excess fractional
  digits would otherwise be silently rounded by Postgres, changing the bill.
- **`occurred_at`** — required; at most 5 minutes in the future (**closes
  ADR-0001's clock-skew open question**: a small tolerance absorbs ordinary
  drift without accepting usage that has not happened yet); at most 35 days old
  (**ADR-0001 §5's backfill bound, now actually enforced**).
- **Unknown fields are rejected.** A client sending `"quanity"` has a bug, and
  silently dropping the field means it is billed as zero while receiving a
  `202` saying everything is fine. This also means a client cannot supply
  `received_at`, which is ours.

Validation takes the current time as a parameter rather than calling
`time.Now()` internally, so the time-based rules are testable at fixed instants.
Invariant I5 — determinism — starts with not reading the clock inside business
logic.

### How the duplicate is detected

```sql
INSERT INTO events (...) VALUES (...)
ON CONFLICT (tenant_id, idempotency_key)
DO UPDATE SET tenant_id = events.tenant_id
RETURNING id, (xmax = 0) AS inserted
```

`DO UPDATE` rather than `DO NOTHING`, despite the no-op `SET`. ADR-0002 already
noted that `DO NOTHING ... RETURNING` yields zero rows on conflict and so
cannot report the existing id. The deeper reason is concurrency: recovering the
id with a follow-up `SELECT` races, because under `READ COMMITTED` the
conflicting row may belong to a transaction that has not committed yet, and the
`SELECT` finds nothing. `DO UPDATE` blocks until that transaction resolves and
then returns the row.

`(xmax = 0)` distinguishes insert from conflict. `xmax` is a Postgres system
column holding the id of the transaction that deleted or locked a row; a
freshly inserted tuple has none and so reads `0`, while the tuple returned from
the `DO UPDATE` path was locked and does not. This is a Postgres implementation
detail rather than standard SQL, and it is the price of learning
insert-versus-conflict in one round trip.

## Consequences

- ADR-0002's Consequences bullet asserting duplicates are indistinguishable is
  superseded by this ADR. ADR-0001's requirement stands and is now specified.
- The no-op `DO UPDATE` writes a new row version on every duplicate, producing
  dead tuples and autovacuum work proportional to duplicate volume. Acceptable
  now; the bounded dedup table (D7) will short-circuit most duplicates before
  they reach this statement.
- A duplicate costs a full write path. Under a client stuck in a retry loop
  that is real load, so ingest metrics must count duplicates separately or a
  retry storm will look like healthy traffic.
- Rejecting unknown fields makes the API strict. Adding a field later is a
  breaking change for any client that was already sending it, which is the
  correct tradeoff for a billing API but should be a conscious one.

## Open questions

- **Same key, different payload (D6)** remains unresolved and is still the most
  likely correctness hole. A client reusing a key for different usage gets a
  `202` and its event silently discarded. Needs a payload fingerprint stored
  with the key; until then this contract is honest about everything except
  that case.
- Whether to store the original response body for true Stripe-style replay
  (D14).
- `meter` is validated as a non-empty string but not against a registry, so a
  typo'd meter name is accepted and silently bills nothing (D12, Phase 4).
