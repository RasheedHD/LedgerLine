# LedgerLine — Project Plan

> **This is the anchor document.** Read it at the start of every session.
> Update it at the end of every session. If something here is wrong, fixing it
> is the highest-priority work in the repo, because everything else is
> navigated from here.

- **Last updated:** 2026-08-03
- **Repo:** `github.com/RasheedHD/LedgerLine`
- **Working agreement:** [CLAUDE.md](CLAUDE.md) — build first, walk through
  after. The tutoring loop is paused; priority is a portfolio-grade result.
- **Decision record:** [docs/adr/](docs/adr/)
- **Session notes:** [docs/learning-log.md](docs/learning-log.md)

---

## 0. How to use this document

Three documents, three jobs. Keep them separate or all three rot:

| Document | Answers | Written when |
|---|---|---|
| **PLAN.md** (this file) | *Where are we going and where are we now?* | Updated at the end of every session |
| **docs/adr/NNNN-*.md** | *Why did we choose this over the alternative?* | Once per design decision, never edited after Accepted except to add a Superseded header |
| **docs/learning-log.md** | *What did I learn?* | Appended every session, under 10 lines |

Rules for this file:

1. **Section 4 (Where we are) must be verifiable.** Never write "done" for
   something not confirmed by a passing test or an observed command output.
2. **Every phase has exit criteria that are testable.** "Ingest works" is not
   an exit criterion. "Two POSTs with the same key produce one row and two
   202s" is.
3. **The debt register (section 7) is append-only until items are closed.**
   Items get IDs so ADRs and commits can cite them.

---

## 1. The thesis

From the README:

> Event streaming + usage billing from scratch in Go. At-least-once delivery
> meets idempotent double-entry accounting with a chaos suite that proves
> invoices stay correct to the cent.

Unpacked, the claim this project exists to prove is:

**A message log can only promise at-least-once delivery. Exactly-once
*delivery* is impossible across a network. But exactly-once *effect* is
achievable, by making the processing side idempotent — and it can be
demonstrated, not just asserted, by breaking the system on purpose and showing
the invoices still balance.**

That sentence is the interview answer this entire repo is built to earn. Every
phase below either builds a piece of it or proves a piece of it.

### What this is not

Scope discipline matters more than features here. Explicit non-goals:

- No authentication, authorisation, or tenant provisioning
- No UI, dashboard, or customer portal
- No payment processing, cards, or money movement
- No currency conversion, tax, or dunning
- No Kubernetes, service mesh, or cloud deployment
- No horizontal scaling of the broker (single-node log is the goal; replication
  is a stretch goal at most)

If a task doesn't serve the thesis sentence, it doesn't belong in this repo.

---

## 2. The invariants

These are the properties that must never break. They are numbered so tests,
ADRs, and commit messages can cite them. **The chaos suite in Phase 7 exists
to assert exactly these under fault injection.**

| ID | Invariant | Why it matters |
|---|---|---|
| **I1** | **Money is conserved.** Every ledger transaction balances: `sum(debits) == sum(credits)`. | This is what double-entry buys. A violation means money was invented or destroyed. |
| **I2** | **No usage is billed twice.** One idempotency key per tenant produces at most one billable event, regardless of how many times it is delivered. | The core promise. At-least-once delivery makes duplicates *certain*, not hypothetical. |
| **I3** | **No accepted usage is silently lost.** Anything that received a `202` either reaches the ledger or lands in a dead-letter with a recorded reason. | Undercounting is worse than overcounting: the customer never complains, so you never find out. |
| **I4** | **Closed invoices are immutable.** Once a period closes, its invoice never changes. | Everything downstream — rev rec, tax, the customer's own books — acted on that number already. |
| **I5** | **Replay is deterministic.** Reprocessing the log from offset 0 against an empty database produces a byte-identical ledger. | This is what makes the system debuggable and auditable. It also makes I1–I4 testable by construction. |
| **I6** | **No float touches money or quantity.** Decimal from wire to storage to arithmetic. | Binary floating point cannot represent decimal fractions; error accumulates until the ledger fails to balance by cents nobody can explain. |

I5 is the strongest and the most demanding: it forbids any non-determinism in
processing — no `time.Now()` in rating, no map iteration order affecting
output, no randomness. It is worth protecting from the start because
retrofitting determinism is close to a rewrite.

---

## 3. Architecture

Target shape at project completion:

```
                    ┌─────────────────────────────────────────┐
   client           │              LedgerLine                 │
     │              │                                         │
     │ POST /events │   ┌──────────────┐                      │
     └──────────────┼──▶│ billing/     │  1. validate         │
                    │   │   ingest     │  2. append to log    │
       202 Accepted │   └──────┬───────┘  3. ack after fsync  │
     ◀──────────────┼──────────┘                              │
                    │          │ append                       │
                    │          ▼                              │
                    │   ┌──────────────┐                      │
                    │   │ broker/log   │  segments + offset   │
                    │   │              │  index, crash-safe   │
                    │   └──────┬───────┘                      │
                    │          │ read from committed offset   │
                    │          ▼                              │
                    │   ┌──────────────┐                      │
                    │   │  consumer    │  at-least-once       │
                    │   └──────┬───────┘  → duplicates HAPPEN │
                    │          ▼                              │
                    │   ┌──────────────┐                      │
                    │   │billing/dedup │  ← I2 enforced here  │
                    │   └──────┬───────┘                      │
                    │          ▼                              │
                    │   ┌──────────────┐                      │
                    │   │billing/      │  meters, plans,      │
                    │   │  pricing     │  tiers — PURE (I5)   │
                    │   └──────┬───────┘                      │
                    │          ▼                              │
                    │   ┌──────────────┐                      │
                    │   │billing/ledger│  ← I1 enforced here  │
                    │   └──────┬───────┘  double-entry        │
                    │          ▼                              │
                    │   ┌──────────────┐                      │
                    │   │  invoicing   │  ← I4 enforced here  │
                    │   └──────────────┘  period state machine│
                    │                                         │
                    │   chaos/  ──breaks──▶ asserts I1–I6     │
                    │   bench/  ──measures──▶ throughput, p99 │
                    └─────────────────────────────────────────┘
```

### Component map

| Path | Responsibility | Status |
|---|---|---|
| `cmd/ingest/` | Process entry point, DB wiring, HTTP server | Skeleton exists |
| `billing/ingest/` | HTTP handler, decode, validate | Minimal, no validation |
| `billing/dedup/` | Idempotency and deduplication | Empty |
| `billing/pricing/` | Meters, plans, rating | Empty |
| `billing/ledger/` | Double-entry posting | Empty |
| `broker/log/` | Segment format, offset index, recovery | Empty |
| `bench/` | Throughput and latency benchmarks | Empty |
| `chaos/` | Fault injection, invariant assertions | Empty |
| `migrations/` | golang-migrate SQL | 1 migration |
| `docs/adr/` | Design decisions | 3 ADRs |

**Note:** `billing/dedup/`, `billing/ledger/`, and `broker/log/` are singled out
in CLAUDE.md as the three components that matter most — the interview
conversation, with everything else supporting them. They get the most care, the
best tests, and the clearest comments. They are Phases 1, 5, and 2.

---

## 4. Where we are today

Verified 2026-08-03 against the working tree.

### Committed and working

- **7 commits**, HEAD at `a4fe384`.
- **Postgres 16** via [docker-compose.yml](docker-compose.yml) — one service,
  named volume `pgdata`, `pg_isready` healthcheck.
- **Migration 000001** creates `events` with the six ADR-0001 fields,
  `id BIGINT GENERATED ALWAYS AS IDENTITY` PK, `quantity NUMERIC(38,9)`,
  both timestamps as `TIMESTAMPTZ`, `UNIQUE (tenant_id, idempotency_key)`, and
  an index on `(tenant_id, occurred_at)`. Applied, reversed, and re-applied
  cleanly with `golang-migrate` in the previous session.
- **`POST /events`** accepts JSON, inserts one row, returns `202 {"id":N}`.
  Verified: a second request with the same key returns `500` and the log shows
  the `23505` unique violation. One row in the table, not two.
- **Three ADRs** — [0001 event schema](docs/adr/0001-event-schema.md),
  [0002 dedup enforcement](docs/adr/0002-dedup-enforcement.md),
  [0003 postgres driver](docs/adr/0003-postgres-driver.md).
- **One dependency:** `jackc/pgx/v5`, used only through `database/sql`.

### Not currently running

Docker Desktop is stopped and nothing is listening on `:8080`. Neither is a
problem — both are started on demand — but it means the database contents are
unverified as of today.

### Ingest is now correct and tested (2026-08-03)

- **A duplicate returns `202` with `{"id":N,"duplicate":true}`** — same id, one
  row. `ON CONFLICT DO UPDATE` with `(xmax = 0)` to tell insert from conflict.
  See [ADR-0004](docs/adr/0004-ingest-api-contract.md).
- **Full validation** ahead of any database work: required fields, decimal
  quantity within `NUMERIC(38,9)`, no negatives, no scientific notation,
  5-minute clock-skew tolerance, 35-day backfill bound, unknown fields
  rejected.
- **Reuse detection.** Every event stores a SHA-256 fingerprint over its
  billable fields. A key returning with different content gets
  `409 idempotency_key_reuse` instead of being silently discarded with a `202`.
  Formatting differences (`"1"` vs `"1.0"`) canonicalise to the same
  fingerprint, so a reformatted retry is still a retry. See
  [ADR-0005](docs/adr/0005-payload-fingerprint.md).
- **64 assertions, all passing**, against a real Postgres. Confirmed genuinely
  running rather than skipped. Mutation-checked twice: breaking duplicate
  detection fails `TestRetryIsIdempotent`, and removing the fingerprint's
  length prefix fails `TestFingerprintIsUnambiguousAcrossFieldBoundaries`. The
  assertions have teeth.
- `TestConcurrentRetriesProduceOneRow` — 50 concurrent goroutines with the same
  key produce exactly one row. This is the test that earns ADR-0002's rejection
  of check-then-insert: a `SELECT`-then-`INSERT` implementation passes the
  simple retry test and fails this one.

### Test harness

`internal/testdb` gives each test a real Postgres, against a **separate
`ledgerline_test` database** so the dev database is never touched. Schema is
dropped and replayed from `migrations/` once per binary; tables are truncated
between tests. Tests **skip** rather than fail when no database is reachable,
which keeps `go test ./...` usable without Docker — at the cost that a green
run proves nothing unless the tests actually ran. CI must assert that.

---

## 5. Phase roadmap

Each phase lists what gets built, which ADRs it produces, and **exit criteria
that are checkable**. Phases are ordered by dependency, not by interest.

---

### Phase 0 — Foundations ✅ COMPLETE

Postgres container, event schema, first migration, ingest skeleton, ADRs 1–3.

---

### Phase 1 — Correct ingest

**Goal:** `POST /events` is genuinely idempotent, validated, and honest about
what it did. This is where **I2** and **I6** first become real.

**Why now:** Everything downstream consumes events. A dishonest ingest layer
poisons the ledger, and no amount of correctness further down recovers it.

**Build:**

1. **Fix the duplicate response.** `INSERT ... ON CONFLICT` rather than a bare
   insert. A retry must get the same answer as the original — see ADR-0002.
   Note the trap already documented there: `ON CONFLICT DO NOTHING RETURNING id`
   returns **zero rows** on conflict, so returning the original id needs either
   a follow-up `SELECT` or a deliberately pointless `DO UPDATE`.
2. **Resolve the ADR-0001 / ADR-0002 contradiction (D2).** ADR-0001's
   Consequences say accepted / duplicate / too-old must be distinguishable.
   ADR-0002 says duplicates return `202` indistinguishable from accepted. Both
   cannot hold. Likely resolution: same status code, distinguishing field in
   the body — but that is a decision to make deliberately, with an ADR.
3. **Validation layer** — required fields, quantity parses as a non-negative
   decimal, `occurred_at` present and not absurdly future (the clock-skew clamp
   from ADR-0001's open questions), `occurred_at` not older than the 35-day
   backfill bound from ADR-0001 §5.
4. **Payload fingerprint (D6).** Same key + different payload currently means
   silent data loss with a success response. Store a hash of the billable
   fields alongside the key and reject on mismatch, as Stripe does.
5. **The bounded dedup table** from ADR-0001 §4 — day-partitioned, 7-day
   retention, `DROP PARTITION` expiry. It sits *in front of* the constraint as
   a fast path, never as the source of truth.
6. **The first tests.** Table-driven, against a real Postgres.

**ADRs:** validation policy and error taxonomy; dedup table shape and payload
fingerprinting; test strategy (real DB vs. mocks).

**Exit criteria:**

- [x] Two identical POSTs → two `202`s, one row, and the response distinguishes
      the second as a duplicate
- [x] Same key + different payload → explicit error, never a silent discard
- [x] N concurrent goroutines POSTing the same key → exactly one row (this is
      the test that proves check-then-insert was correctly rejected)
- [x] Empty body, missing field, negative quantity, malformed timestamp,
      future timestamp, 40-day-old timestamp → each a distinct 4xx, none
      reaching Postgres
- [x] `go test ./...` passes and covers all of the above

**Prepares you to answer:** *How do you make an HTTP endpoint idempotent?
What's actually wrong with SELECT-then-INSERT? Why is a unique constraint
safer than application logic?*

---

### Phase 2 — The broker log

**Goal:** an append-only, crash-safe log with offset-addressed reads, written
from scratch. `broker/log/`.

**Why now:** This is the distributed-systems core of the project and the
hardest single piece. It is independent of billing, so it can be built and
crash-tested in isolation before anything depends on it.

**Build:**

1. **Segment file format** — length-prefixed records with a CRC32 per record,
   a magic number and format version in the segment header. The CRC is what
   makes a torn write *detectable* rather than silently corrupt.
2. **Offset index** — sparse `offset → file position` mapping, so reads don't
   scan from zero. Kafka's `.index` files work this way, and *sparse* is the
   interesting choice: a dense index costs memory proportional to record count.
3. **Append and read** — `Append([]byte) (offset, error)`, `Read(offset)`.
4. **Recovery on open** — scan the tail of the active segment, find the last
   record with a valid CRC, truncate anything after it. **This is where crash
   safety actually lives.**
5. **Segment rolling** — close at a size threshold, open the next.

**The load-bearing decision: fsync policy.** Every append, batched with a
window, or never (relying on the page cache). This is the durability/throughput
dial and there is no free option. Kafka defaults to *not* fsyncing per message
and leans on replication for durability instead — a choice worth understanding
before copying, since this project has no replication.

**ADRs:** segment format and record framing; fsync policy and the exact
durability guarantee `Append` returns; index density.

**Exit criteria:**

- [ ] Write 100k records, close, reopen, read all back identical
- [ ] `kill -9` mid-append → reopen succeeds, every acked record is present,
      no partial record is ever returned to a reader
- [ ] Deliberately corrupt a byte in a segment → the reader detects it via CRC
      rather than returning garbage
- [ ] Read from an arbitrary offset without scanning the whole segment
- [ ] Benchmark: appends/sec at each fsync policy, so the tradeoff is a number
      you measured, not a claim you repeated

**Prepares you to answer:** *How does Kafka store messages on disk? What does
`fsync` actually guarantee, and what does it not? How do you detect a torn
write? Why sparse and not dense indexing?*

---

### Phase 3 — The seam

**Goal:** ingest appends to the log; a separate consumer drains the log into
Postgres. At-least-once delivery becomes real, and **I2 gets proven end to
end** rather than asserted at a single insert.

**Why now:** It needs both Phase 1 (idempotent processing) and Phase 2 (the
log). It is the phase where the thesis sentence stops being theoretical.

**Build:**

1. **Ingest writes to the log, not to Postgres.** The `202` is returned once
   the record is durable *in the log*.
2. **`received_at` moves.** ADR-0001 defines it as "when we took durable
   custody." Today that is the Postgres insert; after this phase it is the log
   append. **This is a semantic change to a field the entire late-event policy
   is built on** and needs an ADR amendment, not a quiet edit.
3. **Consumer** — reads from a committed offset, writes events, commits the
   offset.
4. **Offset commit strategy** — commit before or after processing. Before gives
   at-most-once (lose events on crash, violates I3). After gives at-least-once
   (duplicates on crash, absorbed by Phase 1's dedup). This choice *is* the
   delivery semantics.

**ADRs:** delivery semantics and offset commit ordering; the `received_at`
semantic change.

**Exit criteria:**

- [ ] Kill the consumer mid-batch, restart → every event lands exactly once in
      the ledger, duplicates absorbed by dedup
- [ ] Kill the consumer between processing and offset commit → the replayed
      events are deduped, not double-billed (**this is the money test**)
- [ ] Stop Postgres entirely; ingest keeps accepting into the log and the
      consumer catches up on recovery (I3 under database outage)
- [ ] Replay from offset 0 against an empty database → identical final state
      (**I5**)

**Prepares you to answer:** *At-least-once vs exactly-once — why is
exactly-once delivery impossible, and why is exactly-once processing still
achievable? Where do you commit the offset and why?*

---

### Phase 4 — Pricing

**Goal:** turn quantities into money, deterministically. `billing/pricing/`.

**Build:** meter registry (closing an ADR-0001 open question — is a typo'd
meter name a silent no-op?); plans and price points; tiered and volume pricing;
a pure `Rate(events, plan) → line items` function.

**The constraint that matters:** rating must be **pure**. No `time.Now()`, no
database reads, no map-iteration-order dependence. Same input, same output,
forever. That is what makes **I5** achievable and what makes a disputed invoice
reproducible six months later.

**ADRs:** pricing model and tier semantics; meter registry and unknown-meter
handling.

**Exit criteria:**

- [ ] Table-driven tests over tier boundaries — exactly at, one below, one
      above; zero quantity; a quantity large enough to cross every tier
- [ ] Rating the same input twice produces byte-identical output
- [ ] An unknown meter produces a defined, tested outcome rather than silence
- [ ] No `float64` anywhere in the package (grep it, and put that grep in CI)

**Prepares you to answer:** *How do you price tiered usage? Why must rating be
a pure function?*

---

### Phase 5 — The ledger

**Goal:** double-entry accounting. `billing/ledger/`. **I1 lives here.**

**Build:** chart of accounts; journal entries with balanced postings; a posting
API that makes an unbalanced transaction *impossible to express*, not merely
rejected at runtime; balance queries derived by replaying the journal.

**The idea worth internalising:** double-entry isn't bookkeeping ceremony. It's
a redundancy check — every amount recorded twice, in opposite directions, so
that a single arithmetic or logic error makes the books visibly not balance
instead of silently producing a wrong number.

**ADRs:** chart of accounts and the posting model.

**Exit criteria:**

- [ ] Property test: for any random sequence of postings,
      `sum(debits) == sum(credits)` (**I1**)
- [ ] An unbalanced transaction cannot be constructed through the public API
- [ ] Balance computed from a running total equals balance computed by
      replaying the journal from zero
- [ ] Posting the same priced event twice is rejected or is a no-op (I2 at the
      ledger boundary — defence in depth)

**Prepares you to answer:** *Why double-entry for a billing system? What does
it actually buy you over a balance column?*

---

### Phase 6 — Periods and invoicing

**Goal:** billing periods, invoice generation, and the late-event policy from
ADR-0001 §5 finally implemented. **I4 lives here.**

**Build:** period state machine (`open → closing → closed`); invoice
generation from ledger state; late-event roll-forward — an event whose
`occurred_at` falls in a closed period is accepted, posted to the next open
period, `occurred_at` preserved, rendered as late; enforcement that a closed
invoice cannot be mutated.

**Also:** amend ADR-0001 §5 (**D3**). It says late events are "flagged as
late," but §1's six-field struct has no such field, and the schema decision was
that lateness is *derived*. The ADR still contradicts the migration.

**ADRs:** period state machine and close semantics; invoice immutability
enforcement.

**Exit criteria:**

- [ ] An event arriving after its period closed lands on the next invoice with
      `occurred_at` preserved and is identifiable as late
- [ ] A closed invoice's totals are byte-identical before and after a late
      event arrives for its period (**I4**)
- [ ] An event older than 35 days is rejected with a distinct error, not
      silently dropped and not quietly billed
- [ ] Sum of all invoice line items equals sum of all ledger postings for the
      period (**I1** across the invoice boundary)

**Prepares you to answer:** *What happens to usage that arrives after you've
already invoiced? Why not just reopen the invoice?*

---

### Phase 7 — Chaos and benchmarks

**Goal:** stop claiming the invariants hold and start proving it. This is the
phase the README's last clause is written about, and it is the phase that makes
the project worth talking about.

**Build — `chaos/`:** a harness that runs a workload while injecting faults,
then asserts **I1–I6** after recovery:

| Fault | Invariant under test |
|---|---|
| `kill -9` the broker mid-append | I3, I5 |
| `kill -9` the consumer between process and offset commit | I2 |
| Deliver every message twice, deliberately | I2 |
| Deliver messages out of order | I5 |
| Postgres connection dropped mid-transaction | I1, I3 |
| Client clock skewed hours forward and back | I4, I6 |
| Duplicate keys with divergent payloads | I3 |
| Disk full during segment roll | I3 |

**Build — `bench/`:** ingest throughput and p99 latency; append rate per fsync
policy; end-to-end event → ledger latency; rating throughput at realistic
volumes.

**Exit criteria:**

- [ ] Every fault above runs unattended and asserts invariants automatically
- [ ] The suite catches a deliberately introduced bug (delete the dedup check
      and watch I2 fail) — **an assertion suite that has never failed is
      untested**
- [ ] Benchmarks produce numbers recorded in this file, not just printed
- [ ] "Correct to the cent" is a test name, not a README adjective

**Prepares you to answer:** *How do you know your billing system is correct?* —
and this is the one where a real answer separates you from everyone else in the
interview pool.

---

### Phase 8 — Polish and narrative

**Goal:** make the work legible to someone who has 10 minutes and did not build
it.

**Build:** README rewrite with the architecture diagram and the measured
numbers; structured logging and metrics on ingest; graceful shutdown (**D9**);
`go test ./...` in CI; a Makefile or Taskfile for the common loops;
`docs/interview-narrative.md` — the 5-minute walkthrough, the three hardest
bugs, and the decisions you'd revisit.

**Exit criteria:**

- [ ] A stranger can clone, run one command, and see it work
- [ ] CI runs migrations, tests, vet, and the chaos suite on every push
- [ ] You can walk the whole system in 5 minutes without opening the code

---

## 6. Testing strategy

Currently: none. Target:

| Level | What it covers | Where |
|---|---|---|
| **Unit, table-driven** | Pure logic — rating, validation, framing, CRC | Beside the code |
| **Integration** | Real Postgres, real files. Constraint behaviour, migrations, recovery | `_test.go` with a live DB |
| **Property** | Invariants over generated input — I1 especially | `billing/ledger/` |
| **Chaos** | Invariants under injected faults | `chaos/` |

Conventions, per CLAUDE.md: table-driven by default, and **each case gets a
comment saying what it proves** — a case named `"empty quantity"` is worth
nothing next to one that says *proves an empty quantity never reaches
Postgres.*

Open question to settle in Phase 1: how integration tests get a database.
Options are a throwaway schema per run against the compose container, or
testcontainers (a dependency, and CLAUDE.md requires justifying those).

---

## 7. Debt and open questions register

Append-only. Close items by marking them, not deleting them.

| ID | Item | Source | Phase |
|---|---|---|---|
| ~~D1~~ | ~~Zero tests in the repo~~ — **closed 2026-08-03.** 31 tests against real Postgres; harness in `internal/testdb` | — | 1 |
| ~~D2~~ | ~~ADR contradiction on duplicate distinguishability~~ — **closed 2026-08-03** by ADR-0004: status carries outcome class, body carries detail | ADR-0004 | 1 |
| **D3** | ADR-0001 §5 says late events are "flagged"; §1's struct has no such field and the migration derives it | ADR-0001 | 6 |
| ~~D4~~ | ~~Duplicate key returns `500`~~ — **closed 2026-08-03.** Returns `202` with `duplicate: true` and the original id | ADR-0004 | 1 |
| ~~D5~~ | ~~No validation at all~~ — **closed 2026-08-03.** Full validation before any database work | ADR-0004 | 1 |
| ~~D6~~ | ~~Same key + different payload silently discarded~~ — **closed 2026-08-03** by ADR-0005: SHA-256 fingerprint, `409 idempotency_key_reuse` on mismatch | ADR-0005 | 1 |
| **D7** | Bounded, day-partitioned dedup table not built — **deferred to Phase 7, pending benchmark evidence.** As specified it is a cache in front of the constraint, and adding a lookup before the insert makes the *common* path (a new event) two round trips instead of one. It only pays off once the unique index no longer fits in memory, which is unmeasured. ADR-0002's own principle — "the constraint is the invariant, anything in front of it is a cache" — argues against building a cache with no demonstrated need | ADR-0001 §4 | 7 |
| **D8** | Connection pool entirely at defaults — unbounded connections | ADR-0003 | 1 |
| **D9** | No graceful shutdown; in-flight requests die on exit | code | 8 |
| **D10** | Dev credentials hardcoded as a fallback DSN in `main.go` | code | 8 |
| ~~D11~~ | ~~No clock-skew clamp on `occurred_at`~~ — **closed 2026-08-03.** 5-minute forward tolerance | ADR-0004 | 1 |
| **D12** | No meter registry — a typo'd meter name is a silent no-op | ADR-0001 | 4 |
| ~~D13~~ | ~~35-day backfill bound enforced nowhere~~ — **closed 2026-08-03.** Enforced, with its own `422 event_too_old` | ADR-0004 | 1 |
| **D14** | Whether to store the original response body for Stripe-style replay, or only the id | ADR-0002 | 1 |
| **D15** | Whether dedup belongs before or after the log write — broker's job or billing's | ADR-0002 | 3 |
| **D16** | No CI, no task runner | — | 8 |
| **D18** | Every duplicate writes a dead tuple via the no-op `DO UPDATE`; autovacuum work scales with duplicate volume | ADR-0004 | 1 |
| **D19** | Integration tests skip silently without a database, so a green `go test ./...` can prove nothing. CI must assert they ran | code | 8 |
| **D20** | Docker Desktop leaves undeletable stale socket reparse points on unclean exit, blocking every subsequent start. Cleared by renaming `%LOCALAPPDATA%\Docker\run` and `%LOCALAPPDATA%\docker-secrets-engine` | env | — |
| ~~D17~~ | ~~CLAUDE.md's orphaned three-directory list~~ — **closed 2026-08-03.** Relabelled as "the three components that matter most"; the write-them-myself constraint is retired along with the tutoring loop | CLAUDE.md | — |

---

## 8. ADR index

| ADR | Title | Status |
|---|---|---|
| [0001](docs/adr/0001-event-schema.md) | The UsageEvent schema | Accepted |
| [0002](docs/adr/0002-dedup-enforcement.md) | Deduplication is enforced by a database constraint | Accepted |
| [0003](docs/adr/0003-postgres-driver.md) | pgx as the Postgres driver, used through database/sql | Accepted |
| [0004](docs/adr/0004-ingest-api-contract.md) | The POST /events contract | Accepted |
| [0005](docs/adr/0005-payload-fingerprint.md) | Detecting a reused idempotency key | Accepted |

Planned: validation and error taxonomy · dedup table shape and fingerprinting ·
test strategy · segment format · fsync policy · index density · delivery
semantics and offset commit · `received_at` semantic change · pricing model ·
meter registry · chart of accounts · period state machine.

---

## 9. Glossary

| Term | Meaning here |
|---|---|
| **Event time** | `occurred_at` — when usage happened. Decides billing period. Client-supplied. |
| **Ingest time** | `received_at` — when we took durable custody. Ours. Decides what we knew when. |
| **Lateness** | `received_at - occurred_at`. Every late-data policy is a rule over this number. |
| **Idempotency key** | Client-generated, reused verbatim on retry. Says "if you have this, that one — not a new one." |
| **Offset** | A record's position in the log. Monotonic, immutable, the unit of consumer progress. |
| **Segment** | One file of the log. Rolled at a size threshold. |
| **Sparse index** | `offset → byte position` for *some* records, so reads seek near and scan a little. |
| **Torn write** | A record partially written when the process died. Detected by CRC, never returned to a reader. |
| **At-least-once** | Delivery may repeat, never drop. What a log can actually promise. |
| **Rating** | Turning quantities into money via a plan. Must be pure. |
| **Posting** | One side of a double-entry transaction. Postings within a transaction sum to zero. |
| **Period** | A billing window. Has a state: open, closing, or closed. |

---

## 10. Session checklist

**Starting:**

1. Read section 4 of this file. Is it still true?
2. `docker compose up -d` — Docker Desktop may need starting first
3. `migrate -path migrations -database "$DATABASE_URL" up`
4. Pick the next unchecked exit criterion in the current phase

**Finishing:**

1. Append to [docs/learning-log.md](docs/learning-log.md) — under 10 lines
2. Write the ADR if a design decision was made
3. Update section 4 and tick any exit criteria that are now genuinely met
4. Add any new debt to section 7
5. Commit, small and honest

---

## 11. The immediate next step

**Phase 1 is complete.** Every exit criterion is ticked and every correctness
item in ingest is closed. D7 is deferred to Phase 7 with reasoning recorded in
the debt register — it is an optimisation awaiting evidence, not a missing
guarantee.

**Next: Phase 2, `broker/log/`** — the append-only log. The biggest single
piece in the project and the one the distributed-systems half of the thesis
rests on. In order:

1. **Segment format** — length-prefixed records, CRC32 per record, magic and
   version in the segment header. Get the framing right before anything is
   written on top of it.
2. **Append and read** — the smallest API that works: `Append([]byte) (offset,
   error)` and `Read(offset)`.
3. **Recovery on open** — scan the tail, find the last record with a valid CRC,
   truncate the rest. This is where crash safety actually lives, and it is
   worth writing the crash test before the recovery code.
4. **Sparse offset index** — so reads seek near rather than scanning from zero.
5. **fsync policy** — the durability/throughput dial, and the decision that
   deserves its own ADR with measured numbers rather than a repeated claim.
