# ADR-0009: Delivery semantics and where the consumer offset lives

- **Status:** Accepted
- **Date:** 2026-08-03
- **Deciders:** Rasheed
- **Related:** [ADR-0002](0002-dedup-enforcement.md), [ADR-0008](0008-group-commit.md)

## Context

The broker log promises at-least-once delivery. A consumer reading it has to
decide when to record how far it has got, and that single choice determines
what the system actually guarantees.

The textbook framing is a choice between two bad options:

- **Commit the offset before processing.** A crash in between loses the events.
  At-most-once. Violates **I3** — accepted usage silently lost.
- **Commit the offset after processing.** A crash in between reprocesses them.
  At-least-once. Duplicates, which the unique constraint absorbs.

The received wisdom is to take at-least-once and make processing idempotent,
which this system already does. But the framing hides a third option that is
available whenever the offset and the data live in the same transactional
store.

## Decision

**The offset lives in Postgres, in `consumer_offsets`, and advances in the same
transaction as the inserts it advances past.**

```sql
BEGIN;
  INSERT INTO events ... ON CONFLICT ... ;   -- the batch
  INSERT INTO consumer_offsets ... ON CONFLICT DO UPDATE ...;  -- the offset
COMMIT;
```

Either both land or neither does. There is no window in which events are stored
but the offset is not, or the reverse — so processing is **exactly-once**, not
at-least-once with cleanup afterwards.

This is not a contradiction of the project's thesis, it is the thesis. The log
still only promises at-least-once *delivery*; what changes is that the *effect*
on the database is exactly-once. Delivery and effect are different things, and
conflating them is the mistake the whole system is built to avoid.

### Why not a file, or another store

Because two stores cannot be committed atomically without a distributed
transaction, and reaching for one of those to solve this would be
disproportionate. Putting the offset beside the data means the database's
existing transaction is doing the work for free.

The cost is coupling: this consumer can only write to a store that can commit
its offset alongside its data. A consumer writing to something non-transactional
would have to fall back to at-least-once and lean on idempotency. That is a real
constraint and worth knowing before adding a second consumer.

### Batching

100 records share a transaction by default. One transaction per record would be
simplest but pays a commit — and an fsync inside Postgres — per event. The batch
is still atomic: a failure part-way rolls the whole batch back and it is retried
from the same offset. There is a test that appends an unusable record among ten
good ones and asserts that **nothing** commits, neither rows nor offset.

### Records that can never succeed

A record that fails to decode will fail to decode forever. Blocking on it would
wedge the consumer permanently, so it is counted, logged at error level, and
stepped over.

Invariant I3 says nothing may be lost *silently*. This is loud: it increments a
conflict count and writes a log line naming the offset. It is not yet a
dead-letter queue, which is the right eventual home for it (**D29**).

The same applies to an idempotency key arriving with content different from
what is stored. The event is dropped rather than applied, counted, and logged
with its tenant and key.

## Consequences

- Replaying the log from offset 0 is safe and cheap to reason about: every
  record comes back as a duplicate and the row count does not move. There is a
  test that does exactly this, and it is the clearest end-to-end demonstration
  of **I2** in the project.
- A lost `consumer_offsets` table means a full replay, not data loss. The down
  migration says so explicitly.
- Consumers are named, so a second reader tracks its own position without
  disturbing billing.
- `Stats` distinguishes inserted, duplicate, and conflict. A consumer that only
  reported "processed" would look identical whether it was doing useful work or
  silently dropping every event as a conflict.

## Open questions

- **D29:** conflicts and undecodable records are counted and logged but not
  stored anywhere a human can act on later. A dead-letter table is the obvious
  next step, and is what I3 really asks for.
- The consumer runs `Drain` to completion and returns. Continuous operation —
  tailing the log, backing off when caught up — is not built yet.
- Batch size is untuned. 100 is a guess, not a measurement.
