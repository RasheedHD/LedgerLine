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

## 2026-08-03 (cont.) — group commit

**What we built:** `SyncGroup`. Concurrent appends wait on one shared fsync, so
every acknowledgement is backed by a completed flush without each writer paying
for its own. Closes D26, which gated Phase 3. ADR-0008. 35 tests in broker/log.

**Key decision:** Release the log lock *before* the fsync. Group commit only
works if other writers can append while one is flushing — the batch a sync
covers is exactly the writers who arrived during the previous flush. Holding the
lock across the sync turns group commit back into `SyncAlways` with extra
machinery. Measured: 7.2× `SyncAlways` at 64 writers, ~16 records per fsync,
same durability guarantee.

**Couldn't have written myself yet:** That the syncer must capture the highest
written sequence *before* calling fsync and claim only that much afterwards.
Records written while the flush is in flight may not have been caught by it, so
marking them durable would reintroduce the exact bug in a subtler place. There's
a test where one writer blocks inside the flush and a second arrives mid-sync,
asserting the second is not satisfied by the first's sync. Also learned that
`SyncAlways` gains *nothing* from concurrency — 308/sec at 64 writers vs 312/sec
at one — because throughput there is a property of the disk, not the workload.

## 2026-08-03 (cont.) — the consumer, and exactly-once processing

**What we built:** `billing/event` (shared wire type, JSON codec, fingerprint)
and `billing/consumer` (drains the log into Postgres), plus migration 000003 for
`consumer_offsets`. ADR-0009. 112 tests passing, none skipped.

**Key decision:** The offset lives in Postgres and advances in the *same
transaction* as the inserts it advances past. The textbook choice is
commit-before (at-most-once, loses events) or commit-after (at-least-once,
duplicates) — but that framing hides a third option whenever the offset and the
data share a transactional store. Either both land or neither does, so
processing is exactly-once. The log still only promises at-least-once
*delivery*; what changed is the *effect*.

**Couldn't have written myself yet:** Two bugs that only appeared when running
`go test ./...` instead of one package at a time. Go compiles one binary per
package and runs them **in parallel**, so every package sharing one test
database was dropping and rebuilding the schema under the others — tables
vanishing mid-test, one package's fixtures showing up in another's assertions.
And migration 000002's down used `ALTER TABLE events DROP COLUMN IF EXISTS`,
where `IF EXISTS` guards only the *column*; against an already-dropped table it
failed with 42P01 and left `schema_migrations` dirty, which is exactly the state
a down migration exists to clean up. It had only ever worked because the shared
test database always already had the table.

## 2026-08-03 (cont.) — the double-entry ledger

**What we built:** `billing/ledger` — `Amount` as int64 micro-units, balanced-
by-construction transactions, and a Postgres schema whose deferred constraint
trigger enforces balance independently of Go. Migration 000004. ADR-0010.
171 tests across the repo, none skipped.

**Key decision:** The API accepts only `Transfer{Debit, Credit, Amount}`, never
a bare posting. One transfer necessarily produces one debit and one credit of
equal size, so a transaction balances *by construction* rather than by a
validation step some future code path could skip. `Postings()` returns a copy,
so nobody can unbalance it afterwards. Also chose 6 decimal places, not 2 —
usage billing prices below a cent, so a ledger in cents rounds every posting to
zero and bills nothing.

**Couldn't have written myself yet:** Two things. `DEFERRABLE INITIALLY
DEFERRED` on the constraint trigger — postings land one row at a time, so a
transaction is unbalanced after its first row; an immediate check rejects every
legitimate entry, and deferring to COMMIT is the only moment "balanced" is even
a meaningful question. Proved it by removing DEFERRABLE and watching every
normal post fail. And that my overflow test was wrong, not the code: postings
append as +a, -a, +b, -b, so the running total never exceeds the largest single
transfer and overflow is structurally impossible there.

## 2026-08-03 (cont.) — pricing

**What we built:** `billing/pricing` — meter registry, flat/graduated/volume
models, and a pure `Rate` function. Migration-free; it is all in-memory
arithmetic. ADR-0011. 184 tests across the repo.

**Key decision:** Aggregate usage per meter *before* pricing, not per event.
With tiers the two give different answers — 1500 units arriving as fifteen
events of 100 costs $10.50 aggregated but $15.00 priced individually, because
every event lands in the first tier and never reaches the discount the customer
is entitled to. It also confines rounding to once per meter.

**Couldn't have written myself yet:** Why `Plan.Prices` is a slice and not a map.
Go randomises map iteration order deliberately, so rating that walked a map
would emit line items in a different order every run — and I5 asks for a
byte-identical result. Also that my float-guard test was wrong before the code
was: a byte search for "float64" failed on three *comments* explaining why float
is avoided. Parsing the AST fixes it, because comments aren't in the tree unless
you ask for them.

## 2026-08-11 — D31 resolved: ingest appends to the log

**What we built:** Rewired ingest off Postgres and onto the broker log. It now
validates, appends, and returns `202 {"offset": N}` — no `duplicate` flag, no
`409`. Dedup stays downstream in the consumer. `cmd/ingest` runs both halves,
with graceful shutdown and a 500ms drain ticker. ADR-0012 supersedes parts of
ADR-0004 and ADR-0005. 207 tests.

**Key decision:** Availability decided it. Ingest needs only the log, so usage
keeps being accepted while Postgres is down and the consumer catches up
afterwards — there's a test that posts ten events with nothing consuming, then
drains and finds all ten. The alternative (a synchronous dedup lookup at ingest)
keeps the nicer API but fails every one of those requests, losing the usage at
the client. What it costs: key reuse is now invisible to the caller.

**Couldn't have written myself yet:** That `received_at` had to move, and why
that's an improvement rather than a compromise. ADR-0001 defines it as "when we
took durable custody"; that used to be the Postgres insert and is now the log
append — earlier, closer to when the client actually sent it, so lateness
measurements get *more* accurate. Also that the handler now depends on the log
being opened with `SyncGroup`: a `202` on the strength of an append is only
honest if the append is genuinely durable, and nothing in the type system
enforces that.

## 2026-08-11 (cont.) — dead letters

**What we built:** `dead_letters` (migration 000005) and the consumer writing to
it. A record it cannot apply — reused key, undecodable bytes — is now stored
with the raw log record, reason, tenant and key, in the same transaction that
advances past it. Closes D29. ADR-0013. 213 tests.

**Key decision:** Store the raw record verbatim. Without it a dead letter says
"something failed at offset 412", which nobody can act on; with it the event can
be inspected, explained to the customer, and replayed once the cause is fixed.
That is the difference between I3 being satisfied and merely claimed. Also added
`Stats.Accounted()`, which states I3 as arithmetic: inserted + duplicates +
conflicts must equal read.

**Couldn't have written myself yet:** Why the test I wrote to protect this
passed while the feature was broken. `Drain` accumulates batch stats field by
field and I never added the new `DeadLettered` field, so it was always 0 — and
`TestReplayDoesNotDuplicateDeadLetters` asserts it *is* 0 after a replay, so it
passed happily on a value that was never computed. **An assertion that something
is zero proves nothing unless another test proves it can be non-zero.** Fixed by
making the accumulation a `Stats.add` method so a new field is added in one
place, not at every call site.

## 2026-08-11 (cont.) — posting usage to the ledger

**What we built:** `billing/posting` — the wire between pricing and the ledger.
Reads a tenant's usage for a period, prices it, and posts one balanced
transaction: debit `receivable:<tenant>`, credit `revenue:<meter>`. Closes D35.
ADR-0014. 226 tests.

**Key decision:** The ledger idempotency key is *derived* — `usage:<tenant>:<period>`
— not generated. A random key would post the same revenue again on every run,
and the ledger would balance perfectly while being wrong. That's the failure
double-entry alone cannot catch, because both sides of a duplicate entry are
equally wrong.

**Couldn't have written myself yet:** That account names need namespace
prefixes. Without `receivable:` and `revenue:`, a tenant called "api_calls"
would share an account with the revenue for the api_calls meter, and two
unrelated figures would silently accumulate in one place. Also that half-open
periods `[start, end)` are what stop an event at exactly midnight belonging to
both periods or neither — tested at one nanosecond either side of both bounds.

## 2026-08-11 (cont.) — periods and invoices

**What we built:** `billing/invoicing` — period state machine, invoices, and
closing. Deleted `billing/posting`, whose `Post` would re-bill already-invoiced
events once invoices existed. Migration 000006. ADR-0015. Closes D3, D39, D40.
224 tests.

**Key decision:** `events.invoice_id` with **no lower bound** on the gather
query. Selecting `[start, end)` would leave an event that arrived after its own
period closed unbilled forever, because nobody gathers that window again.
Gathering everything still unbilled up to this period's end means the next run
picks it up — so ADR-0001 §5's late-event roll-forward isn't a special case, it's
just what the query already does.

**Couldn't have written myself yet:** That `SELECT ... FOR UPDATE` was load-
bearing rather than defensive. I assumed concurrent closes were already safe via
the unique constraint on `invoices.period_id`. Removing the lock fails the
concurrency test **3 runs out of 3** — both closes read `state='open'`, both
gather the same events, and the failure lands somewhere downstream where it is
much harder to reason about. Also learned to drop the `closing` state: it only
earns its place when closing is long-running and observable half-done, and a
state nobody can ever see is one that eventually gets handled wrong.

## 2026-08-19 — the chaos suite

**What we built:** `chaos/` — seven scenarios that break the running system and
check I1–I4 held: consumer killed mid-drain, every database connection
terminated, every record delivered twice, the offset rewound, the period close
interrupted mid-transaction, and all of it at once under concurrent load. Each
ends by closing the books and asserting the invoice equals one cent per
acknowledged event. ADR-0016. Phase 7 done.

**Key decision:** Compute the expected total from what ingest *acknowledged*,
using arithmetic that never consults anything the system stored. If the suite
and the system agreed only because both derived the number the same way, the
suite would prove nothing.

**Couldn't have written myself yet:** Both bugs the suite found were in the
suite. The worst was `RewindConsumer` writing the offset unconditionally — when
the consumer was *behind* the chosen value it moved **forward**, skipping
records, losing 11 of 180 acknowledged events, and looking exactly like the
system violating I3. No real fault skips a consumer forward: restored backups
and wiped offset stores all go backward. An unrealistic fault reports bugs that
cannot happen and buries the ones that can. The second: fault injectors calling
`t.Fatal` from their own goroutines, which Go doesn't permit and which turned
the suite's own deliberate connection kills into failures.

## 2026-08-19 (cont.) - CI, and refusing to skip

**What we built:** GitHub Actions on Linux with a Postgres 16 service container:
format, vet, build, `go test -race` over everything including chaos, a no-skips
gate, and a migration round-trip. `.gitattributes` to end the LF/CRLF warnings.
`-short` now skips the chaos scenarios. ADR-0017. Closes D16, D19, D43.

**Key decision:** `LEDGERLINE_REQUIRE_DB` turns a skipped integration test into a
failure. Without it a broken service container produces a *passing* build with
every database test quietly skipped, which is strictly worse than a red build:
the badge then claims the invariants hold when nothing checked them. A green CI
that proves nothing is an active lie; a red one is just bad news. Verified both
ways against a dead port.

**Couldn't have written myself yet:** That CI should apply no migrations. The
instinct is to have the pipeline set up the schema, but then the tests run
against whatever CI created rather than against what `migrations/` describes,
and a broken migration passes. `internal/testdb` replays them itself, and CI
round-trips them in a *separate* step, because "the schema is right" and "the
migrations are reversible" are different claims.
