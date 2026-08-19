# ADR-0016: The chaos suite

- **Status:** Accepted
- **Date:** 2026-08-19
- **Deciders:** Rasheed
- **Related:** [PLAN.md](../../PLAN.md) §2, Phase 7

## Context

Every invariant in PLAN.md was already tested individually, by a unit test that
set up exactly the conditions it needed. That proves each mechanism works. It
does not prove they work **together while something is failing**, which is the
only claim a billing system's users actually care about.

The README has promised, since the first commit, "a chaos suite that proves
invoices stay correct to the cent". This is that.

## Decision

### Assert against an independently computed expectation

Every scenario ends the same way: drain fully, close the period, and check the
invoice equals **one cent per acknowledged event**.

The expected total is arithmetic over what ingest actually answered `202` to,
computed without consulting anything the system stored. If the suite and the
system agreed only because both derived the number the same way, the suite
would prove nothing.

Deriving it from **acknowledgements** rather than from what the test meant to
send matters too: a request that failed for an unrelated reason then makes the
expectation smaller rather than making the suite report a false violation.

### Nothing is mocked

The harness assembles the real HTTP handler, the real segment log with
`SyncGroup`, the real consumer, the real pricing and ledger, against a real
Postgres. A fault injected into a fake only proves the fake handles it.

### The faults

| Fault | What it stands for | Invariant |
|---|---|---|
| Cancel the consumer mid-drain, repeatedly | Deploy, OOM kill, node drain | I2, I3 |
| `pg_terminate_backend` on every connection | Failover, connection reaper | I1, I3 |
| Re-append every log record | At-least-once redelivery | I2 |
| Rewind the committed offset | Restored backup, lost offset store | I2 |
| Cancel a period close mid-transaction | Crash during the billing run | I1, I4 |
| All of the above, concurrently, under load | A bad day | all |

Interrupting the close is the sharpest of these. Closing writes the ledger
entry, the invoice, its line items, the event marks, and the state change —
interrupting it is how you find out whether those are genuinely one
transaction. The scenario checks directly for the shapes a partial close would
leave: a period marked closed with no invoice, an invoice with no ledger
transaction behind it.

### Retries are part of the harness, not a workaround

`DrainFully` and `CloseWithRetries` retry through transient failures. A real
consumer would come back and finish; giving up on the first error would make
every scenario fail for the wrong reason. What is asserted is the state
**after** the system has been allowed to recover, which is the only thing worth
promising.

## Two bugs found — both in the suite

Worth recording, because a chaos suite that reports faults it invented is worse
than no chaos suite.

**The rewind moved forward.** `RewindConsumer` wrote the offset
unconditionally, and when the consumer happened to be *behind* the chosen value
it moved the offset **forward**, silently skipping every record in between.
This produced a genuine loss of acknowledged events — 11 of 180 in the run that
caught it — and looked exactly like an I3 violation in the system.

It was the harness lying. No real fault skips a consumer forward past unread
records: a restored backup, a wiped offset store, and a replayed rebuild all go
backward. Fixed with `LEAST(next_offset, $1)`, so the injector can only ever
move backward.

The lesson generalises: **an unrealistic fault reports bugs that cannot happen
and buries the ones that can.**

**`t.Fatal` on a fault goroutine.** The injectors ran on their own goroutines
and called `t.Fatalf` when their own connection was killed — by the suite's own
`KillDatabaseConnections`, running concurrently. Two problems at once: a
deliberate fault reported as a failure, and `FailNow` called from a goroutine Go
does not permit it from. Both now log and continue.

Neither was found by reading the code. They were found by running the suite
until it failed and making the failure message say where the events had gone.
`Harness.Diagnose` exists because the first failure said only "the total is
wrong", which sends you back to guessing.

## Verified

- Eight consecutive full-suite runs, all passing.
- **Mutation-checked:** removing the `UNIQUE (tenant_id, idempotency_key)`
  constraint fails `TestEveryRecordDeliveredTwice` and `TestEverythingAtOnce`.
  A chaos suite that has never failed is untested.

## Consequences

- The suite takes roughly 20 seconds. Acceptable now; it will need a build tag
  or a short-mode skip before it sits in front of every commit (**D43**).
- Timing-based scenarios are inherently probabilistic. They are seeded where
  randomness is used, but the interleavings a cancel or a connection kill
  produce are not reproducible. A failure may need several runs to reproduce,
  which is why `Diagnose` reports state rather than only a mismatch.
- I5 is not asserted here. Deterministic replay needs a second run against an
  empty database, not an inspection of this one, and it already has coverage in
  `billing/consumer`. I6 is static and enforced by the AST walk in
  `billing/pricing`.
- The faults are all things the system is *designed* to survive. Nothing yet
  injects a fault it should legitimately fail on — disk full during a segment
  roll, or a torn record from power loss (**D44**).

## Open questions

- Should the suite run for a fixed duration rather than a fixed number of
  events, so it explores more interleavings on a slower machine?
- A property-based version — generate a random fault schedule, shrink on
  failure — would be a genuine step up from these hand-written scenarios.
