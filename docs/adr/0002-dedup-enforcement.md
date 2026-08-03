# ADR-0002: Deduplication is enforced by a database constraint

- **Status:** Accepted, superseded in part by [ADR-0004](0004-ingest-api-contract.md)
- **Date:** 2026-07-29
- **Deciders:** Rasheed
- **Related:** [ADR-0001](0001-event-schema.md)

> **Amendment (2026-08-03):** the Consequences bullet below claiming that
> callers cannot distinguish "stored" from "already had it" contradicted
> ADR-0001 and is superseded by ADR-0004. Duplicates return `202` as this ADR
> says, but the body carries a `duplicate` flag. The rest stands.

## Context

ADR-0001 decided that clients generate idempotency keys. It did not decide
*where the check happens* — and that is a separate question with a much worse
failure mode if you get it wrong.

Migration 000001 put `UNIQUE (tenant_id, idempotency_key)` on `events`.
`POST /events` currently does a bare `INSERT` and does not inspect the error,
so a retried key returns `500`. That is a deliberate placeholder, not the
intended behaviour, and this ADR records both the rule and the gap.

## Decision

**The unique constraint is the sole source of truth for "have we seen this
event before." Application code never performs a check-then-insert.**

### Options considered

**1. Check-then-insert in application code.** `SELECT` for the key, `INSERT`
if absent. Rejected, and worth understanding why because it is the option that
feels most natural.

Two concurrent retries of the same request both run the `SELECT`, both find
nothing, and both `INSERT`. The customer is billed twice. This is a
time-of-check-to-time-of-use race, and no amount of care in the application
closes it — the gap between the two statements is where the bug lives. It is
only safe under `SERIALIZABLE` isolation, which means transaction retries and
reduced throughput on the hottest path in the system.

**2. Unique constraint, catch the violation.** Attempt the insert, handle
`23505`. The database serialises contending writers, so the race in (1) is
structurally impossible: one transaction commits, the other is rejected. This
is the current state.

**3. `INSERT ... ON CONFLICT DO NOTHING`.** Same guarantee as (2), one round
trip, and the outcome is reported as rows-affected rather than as an error to
be string-matched. This is where the handler should end up.

**4. External lock (Redis `SETNX`, etc.) before the insert.** Rejected outright.
It creates two systems that can disagree about whether an event exists. If
Redis loses the key, you double-bill. If the insert fails after Redis accepted
the key, you drop usage silently. Two sources of truth for a correctness
invariant is not a design, it is a future incident.

### Chosen

(2) today, moving to (3) in Phase 1.

The principle to hold onto: **the constraint is the invariant; anything in
front of it is a cache.** The bounded, day-partitioned dedup table in
ADR-0001 §4 is a fast path that lets us avoid touching the big table on every
insert. It is allowed to be wrong in the conservative direction. The
constraint is not allowed to be wrong at all.

## The gap this ADR does not yet close

A duplicate currently returns `500`. That is incorrect and known.

The rule the fix must satisfy: **a retry must receive the same response as the
original request.** A client whose connection dropped before the `202` arrived
did nothing wrong. Returning `500` tells it the server is broken; returning
`409` tells it the request was invalid. Both are lies, and both provoke
well-written clients into retrying harder or paging someone.

So the duplicate response is `202`, the same as the original. Stripe goes
further and replays the *original response body*, which requires storing the
response alongside the key — out of scope here, recorded as an open question.

Two wrinkles worth knowing before writing that code:

- `INSERT ... ON CONFLICT DO NOTHING RETURNING id` returns **no rows** on
  conflict, so it cannot tell you the id of the row that already exists. The
  usual workarounds are a follow-up `SELECT`, or a no-op
  `ON CONFLICT ... DO UPDATE SET tenant_id = EXCLUDED.tenant_id RETURNING id`
  that exists purely to make `RETURNING` fire. Neither is obvious; both are
  worth choosing deliberately rather than discovering at 2am.
- Handling `23505` means coupling application code to a Postgres error code.
  Acceptable, but it belongs in one helper function, not scattered across
  handlers, so that swapping databases is a single-file change.

## Consequences

- Dedup correctness does not depend on application code being written
  carefully. It depends on a constraint that cannot be bypassed by any code
  path, including future ones written by someone who has not read this.
- Once duplicates return `202`, callers cannot distinguish "stored" from
  "already had it." That is intentional — under idempotency they should not
  care — but it means the ingest metrics need to count the two separately or
  a client stuck in a retry loop will look like healthy traffic.
- The dedup fast-path table may return false "already seen" only if it is
  wrong in a way that loses usage. It must therefore be checked *before* the
  insert as an optimisation, never used to skip the insert entirely.

## Open questions

- **Same key, different payload.** A client reusing a key for genuinely
  different usage would have its second event silently discarded and be told
  `202`. We would lose billable usage and report success. Stripe detects this
  and returns an error. We currently cannot detect it at all, because the
  constraint only covers the key. Fixing it means storing a payload
  fingerprint alongside the key. This is the most likely correctness hole in
  the current design.
- Whether to store the original response body for true Stripe-style replay,
  or only the id.
- Where the fast-path dedup table lives relative to the broker — before the
  log write or after it — which is really a question about whether dedup is
  the broker's job or billing's.
