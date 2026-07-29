# ADR-0001: The UsageEvent schema

- **Status:** Accepted
- **Date:** 2026-07-29
- **Deciders:** Rasheed

## Context

`UsageEvent` is the seam between the broker and billing. The broker's job is
to accept events durably and hand them to billing in order. Billing's job is
to turn them into ledger entries and eventually an invoice.

Everything downstream inherits whatever this struct gets wrong. If the schema
can't express "this event happened Tuesday but arrived Thursday", late events
become unhandleable. If quantity is a float, the ledger won't balance. So this
gets decided once, up front, and written down.

## Decision

### 1. Fields

```go
type UsageEvent struct {
    TenantID       string    // who is billed
    Meter          string    // what is being counted ("api_calls", "gb_egress")
    Quantity       string    // decimal string on the wire; NUMERIC in the DB
    OccurredAt     time.Time // event time  - when the usage actually happened
    ReceivedAt     time.Time // ingest time - when we took durable custody
    IdempotencyKey string    // client-generated, unique per logical event
}
```

Two timestamps, not one. This is the load-bearing part of the schema.

- `occurred_at` is **event time**. It decides which billing period the event
  belongs to. The client owns it.
- `received_at` is **ingest time**. It decides what we knew and when. The
  broker owns it, stamped at the moment of durable write.

With one timestamp you cannot distinguish "usage in July that arrived in
August" from "usage in August". You would either bill the wrong period or lose
the ability to explain a discrepancy. With both, "late" is expressible as a
first-class property: `received_at - occurred_at` is the lateness of an event,
and every late-data policy in this system is a rule over that number.

This is the same split Kafka Streams and Flink make (event time vs processing
time), and it's why they can do windowing with watermarks at all.

### 2. The client generates the idempotency key

The client generates `idempotency_key` and reuses it verbatim on every retry
of the same logical event. The server never derives one.

The tempting alternative is server-side content hashing:
`hash(tenant_id, meter, quantity, occurred_at)`. It is wrong, and the reason
is worth remembering:

**Two identical events are not the same event.** If a tenant makes two API
calls in the same millisecond, that is genuinely 2 units of usage. Every field
is identical. A content hash collapses them into 1 and silently undercharges.
There is no way to tell a real duplicate from a real repetition by looking at
the payload, because the information that distinguishes them — "this is a
retry of a call I already made" — exists only in the client's head.

The client knows because the client is the one retrying. It sent a request,
the connection died before the response arrived, and it doesn't know whether
we committed. Reusing the key is how it says "if you already have this, that
one, not a new one."

This is exactly Stripe's `Idempotency-Key` header, and structurally the same
as Kafka's idempotent producer, where the producer supplies
`(producer_id, sequence_number)` rather than letting the broker guess.

Consequence we accept: a buggy client that generates a fresh key per retry
will double-bill itself, and we cannot detect it. The alternative fails in the
worse direction (silently undercounting real usage), and undercounting is
harder to notice than a customer complaining about a double charge.

### 3. Quantity is NUMERIC, never float

Binary floating point cannot represent most decimal fractions exactly, so
summing millions of float quantities accumulates error that makes the ledger
fail to balance by cents that nobody can account for.

Practical notes: `NUMERIC` in Postgres, a decimal string on the wire (`"1.5"`,
not `1.5`) because Go's `encoding/json` unmarshals bare JSON numbers into
`float64` and would reintroduce the problem at the parse step.

### 4. Dedup window: 7 days

We retain idempotency keys for 7 days, keyed on `(tenant_id, idempotency_key)`,
and the window is measured from `received_at`.

The tradeoff is fixed and unavoidable:

- **Longer window** → bigger dedup table (more storage, slower lookups, more
  index pressure on the hot ingest path).
- **Shorter window** → a duplicate arriving after the window expires is
  accepted as new usage and the customer is overbilled.

7 days is chosen because the realistic worst case is a client-side outage over
a long weekend: a queue backs up Friday, gets drained Monday or Tuesday, and
replays events whose originals we already have. A 24h window misses that
entirely. Beyond ~a week, a "retry" is almost always a backfill or a
migration, which is an operational event a human should be involved in, not
something we should silently absorb.

Cost sanity check: a key is roughly 100 bytes with index overhead. At 10k
events/sec sustained, 7 days is ~6B keys and ~600GB — too much, which tells us
the dedup table needs partitioning by day so expiry is a `DROP PARTITION`
rather than a `DELETE`, and that at real scale this becomes a bloom filter in
front of the table. Noted here as a constraint for the dedup ADR, not solved
here.

### 5. Events arriving after their invoice closed roll into the next period

An event whose `occurred_at` falls inside a period whose invoice has already
closed is **accepted, posted to the next open period, and flagged as late**.
We do not reject it, and we do not reopen the closed invoice.

The three options were:

1. **Reject it.** Simple, and the customer's usage is silently lost. We
   consumed the resource and then declined to bill for it — the correctness
   failure is on our side, not theirs.
2. **Reopen and restate the closed invoice.** Most "accurate" in a narrow
   sense, and it destroys the one property that makes the rest of the system
   tractable: a closed invoice is immutable. Once invoices can change after
   the fact, every downstream consumer — revenue recognition, tax filings,
   the customer's own accounting, any reconciliation report — has to handle
   retroactive mutation of a number it already acted on.
3. **Roll forward.** The usage is billed, no money is lost in either
   direction, and closed stays closed.

We take (3). The invariant we're protecting is **money is conserved and closed
periods are immutable**, and rolling forward is the only option that keeps both.
It costs us period-level precision: a late July event shows up on the August
invoice. That's why the `late` flag and the preserved `occurred_at` matter —
the line item can be rendered as "API calls (July 14, received late)" so the
invoice remains explainable even though the period is technically wrong.

Bound: events older than **35 days** at ingest are rejected outright with a
distinct error, not silently dropped. Past that horizon this is a backfill,
and a backfill should be an explicit, human-initiated operation rather than
something that quietly lands on a customer's bill two months later. 35 days
is one billing period plus a few days of slack.

## Consequences

- Billing must carry a notion of "open" vs "closed" period; invoice close is a
  real state transition, not just a report being generated.
- Ledger entries need to record both timestamps so a late posting can be
  explained after the fact.
- The dedup table is on the hot ingest path and needs day-partitioning from
  the start. Expiry is `DROP PARTITION`.
- A client that doesn't reuse idempotency keys on retry will double-bill
  itself and we cannot detect it. This needs to be loud in the client docs.
- Rejections (duplicate, too-old) must be distinguishable in the API response.
  "Accepted", "duplicate — already have it", and "too old, use backfill" are
  three different outcomes and a client needs to tell them apart.

## Open questions

- Clock skew: `occurred_at` is client-supplied, so a client with a bad clock
  can place usage in the wrong period or manufacture a future-dated event.
  Probably needs a clamp (reject `occurred_at > received_at + 5m`). Deferred.
- Does `meter` need a registry, or is any string accepted? Affects whether a
  typo'd meter name is a silent no-op. Deferred.
