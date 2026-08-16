# ADR-0013: Dead letters

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Rasheed
- **Related:** [ADR-0009](0009-delivery-semantics.md), [ADR-0012](0012-ingest-appends-to-the-log.md)
- **Resolves:** PLAN.md debt item D29

## Context

Invariant I3 reads: *no accepted usage is silently lost — anything that received
a `202` either reaches the ledger or lands in a dead-letter with a recorded
reason.*

Only the first half was true. Records the consumer could not apply — a reused
idempotency key, an undecodable record — were counted in `Stats` and written to
a log line, then stepped over. A log line is not somewhere anyone can act on
later, and it is gone the moment the process restarts.

ADR-0012 made this materially worse. Ingest no longer sees the events table, so
it cannot tell a client its key was reused; that log line became the **only**
trace that usage had been accepted, acknowledged, and then dropped.

## Decision

**A record the consumer cannot apply is written to `dead_letters`, in the same
transaction that advances past it.**

### The raw record is stored

`record BYTEA NOT NULL` — the log bytes, verbatim.

Without it a dead letter says "something failed at offset 412", which nobody
can act on. With it the event can be inspected, explained to the customer who
asks, and replayed by hand once the cause is fixed. This is the difference
between I3 being satisfied and merely being claimed.

`tenant_id` and `idempotency_key` are stored alongside when known, and are
nullable because an undecodable record by definition has neither. That makes a
dead letter searchable by the customer who complains rather than only by
offset.

### Written in the batch transaction

Same reasoning as ADR-0009's offset. A dead letter written separately leaves two
windows for a crash: advance past a record with nothing recording that it
failed, which is exactly the silent loss I3 forbids; or record a failure for a
record that was never actually skipped. Inside one transaction there is no
window.

There is a test that fails a batch part-way and asserts **neither** the dead
letter nor the offset survives.

### Replay does not accumulate

`UNIQUE (consumer, log_offset)` with `ON CONFLICT DO NOTHING`. Reprocessing the
log re-encounters every failed record; without the constraint the table would
grow on every rebuild and I5 would not hold.

The record is still *counted* as a conflict on replay — it still failed — but no
new row is written. `Stats` distinguishes the two: `Conflicts` is what happened,
`DeadLettered` is what was recorded.

### Dead letters are per consumer

Scoped by name, like offsets. A second reader failing on a record must not look
like billing having failed on it.

## I3 as an arithmetic identity

`Stats.Accounted()` reports whether `Inserted + Duplicates + Conflicts == Read`.

Every record that comes off the log must end in exactly one of three states.
If the identity ever fails, a record vanished with no outcome recorded anywhere
— which is the invariant broken, stated in a form a test can check.
`TestEveryRecordIsAccountedFor` drains a deliberately awkward mixture — new
events, exact retries, a reused key, two undecodable records — and asserts it
holds, then checks the same sum against the database rather than the counters.

## A bug this found, and the test that should have caught it

The first implementation reported `DeadLettered: 0` while the row was really
being written. The cause was not in the dead-letter code at all: `Drain`
accumulates each batch's `Stats` into a running total field by field, and the
new field was never added to that list.

The test that should have caught it — `TestReplayDoesNotDuplicateDeadLetters` —
**passed**, because it asserted `DeadLettered == 0` after a replay and the
broken code returned zero always. An assertion that something is zero passes
just as happily when the value is never computed.

Two changes came out of it. The accumulation is now a `Stats.add` method, so a
new field is added in one place rather than at every call site. And the lesson
is worth stating plainly: **a test asserting a value is zero proves nothing
unless some other test proves it can be non-zero.** `TestKeyReuseIsDeadLettered`
is that other test.

## Consequences

- The dead-letter table grows without bound; nothing prunes it. Deliberate for
  now — these rows are the record that something went wrong, and expiring them
  on a timer is the same mistake ADR-0001 §4 nearly made with billing data.
- `resolved_at` exists and nothing writes it. It is there so "what is still
  outstanding" is a query rather than a spreadsheet, but there is no tooling to
  resolve one yet (**D36**).
- Replaying a dead letter by hand is possible — the bytes are there — but there
  is no command to do it (**D37**).
- A record failing for a *transient* reason still fails the whole batch rather
  than being dead-lettered, which is correct: a database being briefly
  unavailable must be retried, not recorded as a permanent failure. Only
  errors that will never succeed — undecodable, key reuse — are dead-lettered.
  That distinction is currently drawn by which code path the error arrives on
  rather than by inspecting it, which is workable but not principled.

## Open questions

- Should a dead letter alert, rather than waiting to be queried? It is the
  clearest "someone is losing money" signal in the system.
- Should ingest expose a lookup so a client can ask whether a key was applied?
  That restores what ADR-0012 took away without putting a database read on the
  write path.
