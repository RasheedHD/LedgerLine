# ADR-0007: Sparse offset index and the fsync policy

- **Status:** Accepted
- **Date:** 2026-08-03
- **Deciders:** Rasheed
- **Related:** [ADR-0006](0006-segment-format.md)
- **Resolves:** PLAN.md debt items D22, D23

## Context

ADR-0006 left the log working but with two gaps: reads were served from a
**dense in-memory position table**, and `Append` never called `fsync`, so the
log's real guarantee was "survives process death, not power loss."

Both needed deciding with evidence rather than by copying Kafka's defaults,
because this log differs from Kafka in the one way that matters to durability:
**there are no replicas.**

## Part 1: The index is sparse

One index entry per 4 KiB of segment data, not per record. Each entry is 8
bytes — a 4-byte offset and a 4-byte position, both relative to the segment,
which is what keeps them 4 bytes instead of 8 and halves the index.

A dense index costs memory proportional to **record count**: 8 bytes each,
roughly 800 MB at a hundred million records. That is the cost that stops a log
holding more than fits in RAM. A sparse index costs memory proportional to
segment **bytes**, so a 64 MiB segment needs about 16k entries whether it holds
a thousand large records or a million small ones.

Measured: **71 entries for 4000 records**, about 57 records per entry. There is
a test that fails if the index stops being sparse, because a dense one would
pass every other test in the file while quietly reintroducing the problem.

### What it bought

`Open` no longer scans whole segments. Recovery resumes from the last index
entry, so at most one index interval is re-read rather than the entire file:

| Records in log | Open time |
|---|---|
| 1,000 | 990 µs |
| 20,000 | 1.66 ms |

Twenty times the records for 1.7× the time. What remains scales with *segment
count*, not record count — which is the property that was wanted.

### What it cost, and the bug that found

Reads now scan forward from the nearest entry. The first implementation did
one `ReadAt` per record header while skipping, which at ~57 records per
interval meant **~57 syscalls per read**.

| Read implementation | ns/op |
|---|---|
| One `ReadAt` per record header | 86,372 |
| Single buffered pass over the interval | 32,998 |

2.6× faster, and it also cut the whole test suite from 7.0 s to 2.6 s. Worth
recording because the sparse index is normally described as a pure memory win —
in practice it moves cost onto the read path, and how that scan is implemented
matters more than the index design itself.

### The index is derived, never authoritative

The segment is the only authority on what the log contains. A missing,
truncated, or stale index is discarded and rebuilt. Three tests cover this: a
deleted index, an index cut mid-entry, and an index whose entries point past
the end of a truncated segment. Losing the index costs startup time, never
data.

Index entries are written **after** the record they describe, and truncated
alongside the segment during recovery. An entry pointing at bytes that were
never written would survive a crash and send a reader past the end of the file.

## Part 2: The fsync policy

Three policies, measured on 256-byte payloads (AMD Ryzen 7 7730U, Windows):

| Policy | ns/op | Appends/sec | Relative cost | Exposure on power loss |
|---|---|---|---|---|
| `SyncNever` | 13,132 | ~76,000 | 1× | Everything in the page cache |
| `SyncEveryN` (1000) | 20,144 | ~49,600 | **1.5×** | Up to 1000 records |
| `SyncEveryN` (100) | 69,132 | ~14,500 | 5.3× | Up to 100 records |
| `SyncAlways` | 3,204,741 | ~312 | **244×** | None |

**`SyncAlways` costs 244×.** 3.2 ms per append is a real platter-or-flash
round trip, and at ~312 appends/sec it is unusable for an ingest path. This is
the number worth remembering: durability per-record is not expensive, it is
*prohibitive*, and every log design that looks clever is really an answer to
this one measurement.

`SyncEveryN(1000)` is the interesting result — bounded exposure for a 1.5×
cost, an order of magnitude cheaper than syncing every 100.

### Decision

**The library defaults to `SyncNever` and exposes the dial.** A log is a
component; how much durability to buy is the caller's decision, and burying it
in a default would hide the most important tradeoff in the system.

**But the honest note about Kafka:** Kafka defaults to not fsyncing per message
and is right to, because it has replicas on other machines — durability comes
from redundancy rather than from the disk. This log has no replicas. Copying
Kafka's default without copying its replication means copying the tradeoff
without the thing that made it safe.

## The gap this leaves, stated plainly

**`SyncEveryN` acknowledges records that are not yet durable.** With N = 1000,
records 1 through 999 return from `Append` before any sync happens; only the
1000th triggers one. Those 999 acknowledgements are promises the log cannot
keep across a power failure.

That directly threatens **invariant I3** — no accepted usage is silently lost —
once ingest returns `202` on the strength of a log append in Phase 3.

The fix is **group commit**, not a bigger N: concurrent appends wait on the
*same* pending fsync rather than each paying for their own. Throughput then
scales with concurrency while every acknowledgement is backed by a completed
sync. It is how databases have solved this for decades, and it is the right
shape for Phase 3, where many concurrent HTTP requests are exactly the
condition that makes it work.

Recorded as **D26**. Until it exists, the only policy that satisfies I3 is
`SyncAlways`, at 312 appends/sec.

## Consequences

- Reads cost a bounded forward scan. Shrinking `indexIntervalBytes` trades
  memory for read latency; the current 4 KiB is a starting point, not a tuned
  value.
- `SyncEveryN` is safe to use where losing a bounded tail is acceptable, and
  unsafe as the basis for an acknowledgement. The distinction is not visible in
  the API, which is a wart.
- A segment is capped at 4 GiB by the 4-byte index positions. The 64 MiB roll
  threshold keeps this far out of reach, but the two constants are coupled and
  changing one without the other is a bug.

## Open questions

- Should `indexIntervalBytes` adapt to record size? A log of 8-byte records
  scans 500 of them per read; a log of 64 KiB records indexes every record.
- Time-based syncing (every N milliseconds) in addition to count-based, which
  bounds exposure in *seconds* rather than records — usually the unit an
  operator actually reasons about.
