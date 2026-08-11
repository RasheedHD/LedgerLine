# ADR-0012: Ingest appends to the log; deduplication stays downstream

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Rasheed
- **Supersedes in part:** [ADR-0004](0004-ingest-api-contract.md), [ADR-0005](0005-payload-fingerprint.md)
- **Amends:** [ADR-0001](0001-event-schema.md) §1
- **Resolves:** PLAN.md debt item D31

## Context

Phase 3 moves ingest off Postgres and onto the broker log. That single change
makes two promises from earlier ADRs unkeepable, because **ingest can no longer
see the `events` table**:

- ADR-0004 said a duplicate returns `202` with `duplicate: true`.
- ADR-0005 said a key reused with different content returns
  `409 idempotency_key_reuse`.

Both answers live in the database, which is now downstream of a consumer that
may be milliseconds or minutes behind. There is no way to keep them without
ingest querying Postgres synchronously — which is the thing the log was put
there to avoid.

## Decision

**Ingest validates, appends, and answers. Nothing else.**

```
POST /events → validate → log.Append → 202 {"offset": 41}
```

No `duplicate` flag. No `409`. Deduplication and reuse detection happen in the
consumer, against the unique constraint that has enforced them all along.

### Why

**Availability is the entire reason the log exists.** Ingest needs only the log,
so usage keeps being accepted and durably stored while Postgres is slow, being
migrated, or down — and the consumer catches up afterwards. That is invariant I3
doing its job. The alternative design fails every one of those requests and the
usage is lost at the client, which is the failure that actually costs money.

There is a test for exactly this: `TestIngestAcceptsWithoutTheConsumerRunning`
posts ten events with nothing consuming, confirms all ten are durable on the
log and zero rows exist, then drains and finds all ten.

**A retry is still safe.** The client's guarantee was never "you will be told
whether this was a duplicate", it was "retrying will not bill you twice". That
holds unchanged — it just holds one component later. ADR-0002 made this exact
argument before ADR-0004 overrode it: under idempotency the caller should not
need to care.

### What it costs, stated plainly

**Key reuse is now invisible to the client.** A client that recycles an
idempotency key for genuinely different usage gets a `202`, and its event is
dropped by the consumer. We lose billable usage *and* report success — the
undercounting failure ADR-0005 was written to close, reopened at the HTTP
boundary.

It is not undetectable, only un-*answerable in the response*: the consumer
counts it in `Stats.Conflicts` and logs it at warn level with the tenant and
key. That is an operational signal rather than a client-facing one, and it is
weaker. **D29** — a dead-letter table — is what turns it back into something a
human can act on, and it matters more now than it did before this ADR.

### The response body

`{"offset": 41}`. The log offset is the only identifier ingest can honestly
hand out: the database row does not exist yet and might not for another few
milliseconds. Returning a row id would mean inventing one.

The rejection taxonomy from ADR-0004 is otherwise unchanged — `400` for
malformed and invalid, `413` for oversized, `422 event_too_old` for the backfill
window. Those are all decidable from the request alone, which is exactly why
they survive.

### `received_at` moves

ADR-0001 defines `received_at` as "when we took durable custody". That moment
was the Postgres insert; it is now the **log append**. The event is safe once it
is in the log, whether or not the database has caught up.

This is a semantic change to the field every late-event decision is arithmetic
on, so it is recorded here rather than made quietly. In practice the value moves
*earlier* — closer to when the client actually sent the event — which makes
lateness measurements more accurate, not less.

### The log must use `SyncGroup`

Ingest returns `202` on the strength of the append, so the append has to be
genuinely durable before it returns. Under `SyncNever` or `SyncEveryN` the
record may still be only in the page cache and the `202` becomes a promise that
does not survive power loss — I3 broken by construction.

`NewHandler` documents this and `cmd/ingest` wires it. It is not enforced by the
type system, which is the same gap as **D28**.

## Verified

Against the running binary, not only in tests:

```
attempt 1: {"offset":0}   [HTTP 202]
attempt 2: {"offset":1}   [HTTP 202]     <- same idempotency key
attempt 3: {"offset":2}   [HTTP 202]     <- same idempotency key
different key: {"offset":3}   [HTTP 202]
negative quantity: {"error":{"code":"invalid_field",...}}   [HTTP 400]

events table: 2 rows (live-1, live-2)
consumer_offsets: billing → 4
```

Three retries, three log records, one row. That gap is the design.

## Consequences

- `billing/ingest` no longer imports `database/sql`. Its tests need no
  database, which is why that package's suite dropped from ~9s to ~1.7s.
- The consumer is now the only writer to `events`, so the dedup logic exists in
  exactly one place instead of two.
- Row ids in `events` have gaps. `ON CONFLICT DO UPDATE` consumes an identity
  value even when it does not insert, and Postgres does not roll sequences
  back. Cosmetic, but surprising if you assume ids are dense.
- Ingest and the consumer are separable into two processes whenever wanted;
  they share only the log directory and the database. `cmd/ingest` runs both
  for convenience.
- `cmd/ingest` deliberately does **not** fail to start when Postgres is
  unreachable. It warns. Refusing to boot would discard the availability this
  ADR is entirely about.

## Open questions

- Should there be a way for a client to *ask* whether a key was accepted — a
  `GET /events/{key}` that queries the events table? That restores the
  information without putting a database read on the write path. Not built.
- `D29` (dead-letter for conflicts and undecodable records) is now the main
  outstanding I3 gap.
