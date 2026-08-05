# ADR-0008: Group commit

- **Status:** Accepted
- **Date:** 2026-08-03
- **Deciders:** Rasheed
- **Related:** [ADR-0007](0007-index-and-durability.md)
- **Resolves:** PLAN.md debt item D26

## Context

ADR-0007 measured the durability dial and left a hole in it, recorded as D26:

> `SyncEveryN` acknowledges records that are not yet durable. With N = 1000,
> records 1 through 999 return from `Append` before any sync happens.

That is harmless while nothing acknowledges on the strength of an append. It
stops being harmless in Phase 3, where ingest returns `202` once the record is
in the log. At that moment the API is promising durability the log has not
delivered, and **invariant I3 — no accepted usage is silently lost — is broken
by construction.**

So the choice looked like:

- `SyncAlways`: every acknowledgement backed by a real fsync, at ~312
  appends/sec. Correct and unusable.
- `SyncEveryN`: fast, and lies.

## Decision

**Neither. Add `SyncGroup`: concurrent appends wait on one shared fsync.**

An fsync flushes everything written to the file before it. If fifty writers
have each appended and all are waiting, one sync makes all fifty durable. Every
acknowledgement is still backed by a completed flush — the guarantee is
identical to `SyncAlways` — but the cost is divided among everyone in the
batch, so throughput rises with concurrency instead of being capped at one
fsync per record.

This is not a new idea. Postgres, InnoDB, and essentially every write-ahead log
since the 1980s do the same thing under the same name. It is worth
understanding rather than importing, because the correctness rests on one
detail that is easy to get wrong.

### The rule that makes it correct

**A syncer captures the highest written sequence *before* calling fsync, and
claims only that much afterwards.**

Records written while the flush is in flight may or may not have been caught by
it — there is no guarantee fsync includes a write that landed after it started.
Claiming them would mark records durable that are not, which is precisely the
bug this ADR exists to remove, reintroduced in a subtler place. Those writers
wait for the next round instead.

There is a test that pins this down: one writer blocks inside the flush while a
second arrives mid-sync, and the second must not be satisfied by the first
writer's sync.

### The other detail: the lock must be released before the fsync

Group commit only works if other writers can append *while* one of them is
flushing — the batch a sync covers is exactly the set of writers that arrived
during the previous flush. Holding the log lock across the sync would serialise
appends behind it and reduce group commit to `SyncAlways` with extra machinery
and worse latency.

`Append` therefore releases the log lock, then waits on the coordinator.

### Segment rolls

A roll flushes the outgoing segment while holding the log lock, and marks
everything appended so far as durable. Without that, a writer whose record
landed in a previous segment could be woken by a sync that never touched the
file its record is in.

## Measured

64 concurrent writers, 256-byte payloads, AMD Ryzen 7 7730U on Windows:

| Policy | ns/op | Appends/sec | Durable on return? |
|---|---|---|---|
| `SyncNever` | 17,512 | ~57,000 | No |
| `SyncAlways` | 3,242,420 | ~308 | Yes |
| `SyncGroup` | 452,222 | ~2,211 | **Yes** |

**`SyncAlways` gains nothing from concurrency.** ~308 appends/sec with 64
writers against ~312 with one, because each writer pays for its own flush and
they queue. That flatness is the whole argument: throughput under
`SyncAlways` is a property of the disk, not of the workload.

`SyncGroup` reaches **7.2× `SyncAlways`** at the same guarantee, batching 15.9
records per fsync. Under the unit test's heavier contention it reached 13.8
records per sync across 1280 appends with 64 writers.

The batching ratio is not fixed — it rises with concurrency, because more
writers arrive during each flush. That is the property that makes this scale
where a larger `SyncEveryN` does not: `SyncEveryN` trades correctness for
throughput at a fixed rate, group commit buys throughput from concurrency and
gives up nothing.

## Consequences

- `SyncGroup` is the policy for anything that acknowledges. Ingest in Phase 3
  must use it; `SyncEveryN` must not be used behind a `202`.
- Latency under `SyncGroup` is worse than `SyncNever` and better than
  `SyncAlways`: a writer waits for at most the flush in progress plus its own.
  Throughput improves with load, which is the opposite of the usual shape and
  worth knowing when reading a latency graph.
- A failing sync reaches every waiter rather than one. That is correct — a disk
  that cannot flush is not a per-request problem — but it means one hardware
  fault surfaces as a burst of errors.
- `SyncCount` is exposed so batching can be asserted rather than assumed. A
  test that only checked throughput would pass if group commit silently
  degraded into `SyncAlways` on a fast disk.

## Open questions

- A small delay before flushing would enlarge batches under bursty load, at the
  cost of latency when idle. Classic group-commit tuning; not worth adding
  without a workload to tune against.
- `SyncEveryN` remains in the API and is still unsafe behind an
  acknowledgement. The type system does not stop that mistake. A `Durable()
  bool` method on the policy, or splitting the enum, would make the distinction
  impossible to miss.
