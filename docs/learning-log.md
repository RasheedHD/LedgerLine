# Learning log

## 2026-07-29 — ADR-0001, the UsageEvent schema

**What we built:** No code. Wrote down the event schema that sits between the
broker and billing: tenant_id, meter, quantity, occurred_at, received_at,
idempotency_key.

**Key decision:** The client generates the idempotency key, not us. The
alternative — server-side content hashing — is rejected because two genuinely
identical events (same tenant, meter, quantity, millisecond) are real usage,
not a duplicate, and a hash collapses them into one and undercharges. Only the
client knows whether it is retrying.

**Couldn't have written myself yet:** The late-event policy. I knew the two
timestamps mattered but not that "closed invoices are immutable" is the
invariant you protect, and that rolling late events into the next period is the
only option that keeps both that and money-conservation.

## 2026-07-29 — Postgres container + migration 000001

**What we built:** A Postgres 16 dev container, and the first golang-migrate
migration creating the `events` table from ADR-0001. Verified the unique
constraint by hand: a retried idempotency key is rejected, identical usage
under a *different* key is accepted, and two tenants can reuse the same key
string without colliding.

**Key decision:** `events` keeps usage forever with an unbounded
`UNIQUE (tenant_id, idempotency_key)`, rather than being the 7-day partitioned
dedup table ADR-0001 §4 describes. Writing the DDL exposed that §4, taken
literally with one table, means dropping billing records on a timer. The
bounded dedup structure becomes a *separate* table later; this constraint is
the backstop under it.

**Couldn't have written myself yet:** Why `received_at` gets no `DEFAULT now()`.
A default looks like a safety net but would mask the bug where the app forgot
to set it — the insert succeeds with a plausible timestamp and every lateness
calculation downstream is silently wrong.

## 2026-07-29 — POST /events, first Go code

**What we built:** `cmd/ingest/main.go` and `billing/ingest/handler.go`. Plain
`net/http`, no framework. Decode JSON, insert one row, return 202. No dedup,
no validation. First dependency: `jackc/pgx/v5`, used only through
`database/sql`.

**Key decision:** Handler does a bare INSERT and does not inspect the error, so
a repeated idempotency key comes back as a 500. Rejected the alternative
(`ON CONFLICT DO NOTHING`, return 202 twice) because that is already the fix,
and I wanted to see the failure first. My plan predicted two duplicate rows;
what actually happens is one row and a rejected insert — the unique constraint
from migration 000001 catches it. The real bug is not "duplicate row", it is
"a correct client retried and got a 500".

**Couldn't have written myself yet:** That `sql.Open` doesn't connect. It only
validates the DSN and builds a lazy pool, so without an explicit `Ping` at
startup a dead database first shows up as a failing request and reads like a
handler bug.

## 2026-08-03 — PLAN.md, then Phase 1 ingest correctness

**What we built:** PLAN.md as the project anchor — invariants I1–I6, eight
phases, debt register. Then the first tests in the repo (31, against a real
Postgres via `internal/testdb`), full request validation, and the duplicate fix:
`202` with `{"duplicate":true}` and the original id instead of a `500`.

**Key decision:** ADR-0001 and ADR-0002 contradicted each other on whether a
duplicate is distinguishable from a fresh accept. Resolved in ADR-0004 by
noticing they were arguing about different layers — status code carries the
outcome class (`202`, stop retrying), body carries the detail (`duplicate`).
Rejected `409`, which tells a correct client its request was invalid.

**Couldn't have written myself yet:** Why `ON CONFLICT DO UPDATE` beats
`DO NOTHING` plus a follow-up `SELECT`. Not just that `DO NOTHING ... RETURNING`
yields no rows — the `SELECT` also *races*: under READ COMMITTED the conflicting
row may belong to an uncommitted transaction, so it finds nothing. `DO UPDATE`
blocks until that transaction resolves. Also learned `(xmax = 0)` as the way to
tell an insert from a conflict in one round trip.

## 2026-08-03 (cont.) — payload fingerprinting, Phase 1 done

**What we built:** Migration 000002 and `fingerprint.go`. Every event stores a
SHA-256 over its billable fields; a key returning with different content now
gets `409 idempotency_key_reuse` instead of being silently dropped with a `202`.
ADR-0005. That was the last open Phase 1 exit criterion.

**Key decision:** Length-prefix every field before hashing. Plain concatenation
would make tenant "ab" + meter "c" hash the same as tenant "a" + meter "bc", so
two different events could share a fingerprint and a reused key would slip
through — reintroducing the exact bug. A delimiter doesn't fix it, since any
byte can appear inside a field. Also decided to canonicalise quantity ("1" and
"1.0" hash alike), otherwise a client reformatting its retry gets falsely
accused of reuse.

**Couldn't have written myself yet:** That the fingerprint returned by
`ON CONFLICT DO UPDATE ... RETURNING` is the *stored* one, not the one just
offered — because the no-op `SET` touches only `tenant_id`, every other column
in the returned row still holds its original value. That property is what makes
reuse detectable in a single statement.

## 2026-08-03 (cont.) — broker/log: framing, segments, recovery

**What we built:** The append-only log. Length+CRC32C record framing, segment
files with a magic/version header rolling at 64 MiB, offset-addressed reads,
and crash recovery that truncates a damaged tail. 25 tests including a real
`kill -9` of a child process mid-append. ADR-0006.

**Key decision:** The checksum covers the *length field*, not just the payload.
Checksumming only the payload leaves the length unprotected — and a flipped bit
there doesn't corrupt one record, it desynchronises the reader from the whole
stream so every following record is read from the wrong position. Covering the
length keeps the failure local. There's a test that fails if you exclude it.

**Couldn't have written myself yet:** That `kill -9` *cannot* produce a torn
record. I assumed it would. Measured across repeated runs: the tail always ends
on a clean record boundary, because the kernel finishes the `WriteAt` syscall
before delivering the kill and Go retries short writes internally. A torn record
needs power loss, not process death — which is exactly the distinction fsync is
about, and the reason the truncation test is the one that actually exercises
repair.

## 2026-08-03 (cont.) — sparse index and the fsync numbers

**What we built:** The sparse offset index (one entry per 4 KiB, not per
record — 71 entries for 4000 records) and the fsync dial, both benchmarked.
Phase 2 done: 30 tests. ADR-0007.

**Key decision:** Default stays `SyncNever` and the dial is exposed, because a
log is a component and durability is the caller's call. The number that decided
it: `SyncAlways` costs **244×** — 3.2 ms per append, ~312 appends/sec —
while `SyncEveryN(1000)` costs only 1.5×. Also learned why Kafka can default to
no-fsync and we can't: Kafka's durability comes from replicas on other machines,
and this log has none.

**Couldn't have written myself yet:** Two things. That a sparse index moves cost
onto the *read* path rather than being a pure memory win — my first version did
one syscall per record header while scanning, ~57 per read; buffering the
interval took reads from 86 µs to 33 µs. And that `SyncEveryN` is not a
durability policy at all for an API that acknowledges: records 1–999 return
before any sync happens. The real answer is group commit, where concurrent
appends wait on one shared fsync.
