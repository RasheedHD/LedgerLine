# Interview narrative

Revision material. The learning log records what happened session by session;
this assembles it into the shape a conversation actually takes.

Read the numbered answers out loud once. Most of them fall apart if you have
only read them.

---

## The 60-second version

> LedgerLine is a usage-billing system built on an append-only log I wrote from
> scratch — segment files, CRC framing, a sparse offset index, crash recovery.
> Usage events come in over HTTP, get appended to that log, and a consumer
> drains them into Postgres, prices them, and posts them to a double-entry
> ledger.
>
> The interesting part is the guarantee. A log can only promise at-least-once
> delivery, so duplicates are certain rather than hypothetical. Exactly-once
> *effect* comes from making the processing side idempotent. And rather than
> assert that, there's a chaos suite that kills database connections, cancels
> the consumer mid-batch, rewinds its offset and replays the log — then closes
> the books and checks the invoice is right to the cent.

Then stop. Let them pick where to go.

---

## The five-minute walkthrough

Follow the data. It is the only order that doesn't require backtracking.

**1. A request arrives.** `billing/ingest` validates it and appends to the log.
It returns `202` with a log offset — and deliberately **cannot** tell you
whether it was a duplicate, because it never touches Postgres. That's what lets
it keep accepting usage while the database is down.

**2. The append is durable before the 202.** Under group commit, concurrent
requests wait on one shared `fsync`. Acknowledging something that isn't on disk
would be a promise the system can't keep.

**3. The consumer drains the log.** It advances its committed offset **in the
same transaction** as the rows it writes. Anything it can't apply — a reused
idempotency key, an undecodable record — goes to a dead-letter table with the
raw bytes.

**4. Pricing is a pure function.** No clock, no database, no map iteration
order. Usage is aggregated per meter *before* pricing, because with tiered
prices, pricing per event puts everything in the first tier.

**5. The ledger is double-entry.** An unbalanced transaction can't be
*expressed* — the API only accepts transfers, which name both sides. Postgres
enforces balance independently with a deferred constraint trigger.

**6. Closing a period issues an invoice**, marks the events it billed, and posts
the revenue — one transaction. Closed invoices are immutable, enforced by
triggers that raise unconditionally.

**The demo:** post the same event twice. Two `202`s, two log records, **one row**
in `events`, and one cent on the invoice. That gap is the whole design.

---

## The three hardest bugs

These are the stories worth having. Each has a lesson that generalises.

### 1. The test passed while the feature was broken

Adding dead letters, the consumer reported `DeadLettered: 0` while the row was
genuinely being written. The cause wasn't in the dead-letter code at all —
`Drain` accumulates each batch's stats field by field, and the new field was
never added to that list.

**The test written to protect this passed.** It asserted `DeadLettered == 0`
after a replay, and the broken code returned zero *always*. It passed happily on
a value that was never computed. Three other dead-letter tests passed too.

> **The lesson:** an assertion that a value is zero proves nothing unless
> another test proves it can be non-zero.

Fixed by making the accumulation a `Stats.add` method, so a new field is added
in one place instead of at every call site — the shape of the code caused the
bug, not carelessness.

### 2. The chaos suite injected a fault that cannot happen

The grand-finale chaos scenario failed: on the run I diagnosed, 169 events
billed against 180 acknowledged. It looked exactly like the system silently
losing usage, which is the one failure the whole project is built to prevent.

It was the harness lying. `RewindConsumer` wrote the offset unconditionally, so
when the consumer happened to be *behind* the chosen value, the "rewind" moved
it **forward** — silently skipping every record in between. No real fault does
that: restored backups, wiped offset stores and rebuilds all go backward.

> **The lesson:** an unrealistic fault reports bugs that cannot happen and
> buries the ones that can.

Worth adding: it was only diagnosable because I made the failure message say
where the events had gone. The first version said "the total is wrong", which
sends you straight back to guessing.

### 3. Parallel test packages sharing one database

Everything passed package by package. Running `go test ./...` failed in ways
that made no sense — tables vanishing mid-test, one package's fixtures showing
up in another's assertions.

Go compiles **one binary per package and runs them in parallel**. Every package
was dropping and rebuilding the schema in the same test database, underneath the
others. Each package now gets its own, named after its path.

The same run exposed a second bug: migration 000002's down used
`ALTER TABLE events DROP COLUMN IF EXISTS`, where `IF EXISTS` guards only the
*column*. Against an already-dropped table it failed and left the migration
state dirty — the exact state a down migration exists to clean up. It had only
ever worked because the shared database always already had the table.

> **The lesson:** a suite that has only ever been run one way has only been
> tested one way.

---

## Two things I was wrong about

Say these unprompted if the conversation allows. Being able to name where your
intuition failed is worth more than another correct answer.

**`kill -9` cannot produce a torn record.** I built a crash test assuming it
would, then instrumented it to check — and across every run the log's tail ended
on a clean record boundary. The kernel completes the `write` syscall before
delivering the signal, and Go retries short writes internally. **A process
cannot be interrupted partway through its own write.** Torn records need power
loss, which is exactly the distinction `fsync` is about. So the crash test
proves a killed writer never leaves an unreadable log, and a *separate* test
that constructs the damage directly is the only thing exercising the repair
path.

**An overflow test that was wrong, not the code.** I asserted that several
maximum-sized ledger transfers would overflow `int64`. It failed. Postings are
appended as `+a, -a, +b, -b`, so the running total alternates between one
transfer's amount and zero — it never exceeds the largest single transfer.
Overflow is *structurally impossible* there. The checked arithmetic still earns
its place for callers accumulating **per account**, where a total genuinely can
run past `int64`.

---

## Questions you will be asked

### Distributed systems

**"Exactly-once delivery — is that real?"**
No. Exactly-once *delivery* is impossible across a network: the sender can never
know whether a lost acknowledgement means the message arrived. Exactly-once
*effect* is achievable by making the receiver idempotent. Here the log gives
at-least-once and the consumer's unique constraint collapses the duplicates.

**"Where do you commit the consumer offset?"**
The textbook framing is commit-before (at-most-once, loses events) or
commit-after (at-least-once, duplicates). That framing hides a third option
whenever the offset and the data share a transactional store: I commit the
offset **in the same transaction** as the rows, so either both land or neither
does. There's no window at all. The cost is coupling — this consumer can only
write somewhere that can commit its offset alongside its data.

**"How does the log store records?"**
Length-prefixed records with a CRC32C, in segment files that roll at 64 MiB,
with a sparse offset index — one entry per 4 KiB, not per record. **The checksum
covers the length field, not just the payload.** A flipped bit in an unprotected
length doesn't corrupt one record; it desynchronises the reader from the entire
stream, so every subsequent record is read from the wrong position.

**"Why sparse and not dense indexing?"**
A dense index costs memory proportional to *record count* — 8 bytes each, about
800 MB at 100 million records. That's what stops a log holding more than fits in
RAM. Sparse costs memory proportional to *bytes*, so the cost is the same
whether a segment holds a thousand large records or a million small ones. The
tradeoff is a bounded forward scan on read.

Worth adding: it's usually described as a pure memory win, but it **moves cost
onto the read path**. My first implementation did one syscall per record header
while scanning — about 57 per read. Buffering the interval took reads from 86 µs
to 33 µs.

**"What does fsync cost?"**
Measured, 256-byte payloads: no fsync ~76,000 appends/sec; fsync per append
**~312/sec**. That's 244×. The number that matters more: **fsync-per-append
gains nothing from concurrency** — 308/sec with 64 writers against 312 with one,
because its throughput is a property of the disk, not the workload. Group commit
reaches ~2,211/sec at the *same* guarantee, because concurrent appends wait on
one shared flush.

**"Kafka doesn't fsync by default — why do you?"**
Because Kafka buys durability from **replicas on other machines**. This log is
single-node. Copying its default without copying its replication copies the
tradeoff without the thing that made it safe.

**"How does group commit work, and where's the subtlety?"**
Many writers wait on one `fsync`, which flushes everything written before it. Two
details carry the correctness. First, **the syncer captures the highest written
sequence before calling fsync and claims only that much** — a record written
while the flush is in flight may not have been caught by it. Second, **the lock
is released before the fsync**, because the batch a sync covers is exactly the
writers who arrived during the previous flush; holding the lock reduces group
commit to fsync-per-append with extra machinery.

### Billing correctness

**"Why double-entry?"**
It's a redundancy check, not bookkeeping ceremony. Every amount is recorded
twice in opposite directions, so a single arithmetic or logic error makes the
books visibly fail to balance instead of quietly producing a wrong number.

**"How do you stop an unbalanced transaction?"**
The API can't express one. It accepts `Transfer{Debit, Credit, Amount}` — never
a bare posting — so one transfer necessarily produces one debit and one credit
of equal size, and a transaction balances *by construction* rather than by a
validation step a future code path might skip. Postings are returned as a copy
so nobody can unbalance it afterwards. Postgres enforces it independently too,
because a migration or repair script bypasses the Go code entirely.

**"That database trigger — why deferred?"**
Postings insert one row at a time, so a transaction is unbalanced after its
first row and only becomes balanced when the last lands. An immediate check
rejects **every legitimate entry**. Deferring to `COMMIT` means the rule is
evaluated when the whole entry is present, which is the only moment "balanced"
is even a meaningful question. I proved it by removing `DEFERRABLE` and watching
every normal post fail.

**"Why not floats?"**
Binary floating point can't represent most decimal fractions, so summing
millions of postings accumulates error until the books fail to balance by cents
nobody can explain. Money is an `int64` count of micro-units — six decimal
places, not two, because usage billing prices below a cent and a ledger in cents
rounds every individual posting to zero. It's enforced by a test that walks the
AST of every file under `billing/`.

Worth adding: the first version of that test searched file bytes and failed
immediately — on three *comments* explaining why float is avoided. Parsing the
AST draws the line in the right place, because comments aren't in the tree.

**"What happens to usage that arrives after you've invoiced?"**
It rolls into the next period, keeping its original event time and flagged late.
Rejecting it loses usage we actually served; reopening the invoice destroys the
one property everything downstream depends on — rev rec, tax, the customer's own
books all acted on that number. Rolling forward is the only option that keeps
both money-conservation and immutability.

The implementation is the nice part: the gather query selects everything
*unbilled* up to the period end, with **no lower bound**. So a late event is
simply still unbilled and the next run picks it up — the policy falls out of the
query rather than needing a special case.

**"Who generates the idempotency key?"**
The client. Server-side content hashing is the tempting alternative and it's
wrong: two API calls in the same millisecond are genuinely 2 units of usage with
every field identical, and a hash collapses them into 1 and undercharges. The
information that distinguishes a retry from a repetition — "I already sent this"
— exists only in the client's head.

**"So a client can reuse a key for different data?"**
Yes, and that used to be silently accepted. Every event now stores a SHA-256 of
its billable fields; a key returning with different content is refused.
**Each field is length-prefixed before hashing** — plain concatenation makes
tenant `"ab"` + meter `"c"` hash identically to tenant `"a"` + meter `"bc"`, so
two different events could share a fingerprint and a reused key would slip
through. A delimiter doesn't fix it, since any byte can appear inside a field.

### Testing

**"How do you know it's correct?"**
Three layers. Unit tests for pure logic. Integration tests against a **real
Postgres**, not mocks — every correctness claim here lives in a constraint, a
transaction or a type, and a mocked constraint proves nothing about whether the
constraint exists. Then the chaos suite, which breaks the running system and
checks the invoice is still right to the cent, against an expectation computed
independently of anything the system stored.

**"How do you know the tests are any good?"**
The load-bearing ones are mutation-tested: I deliberately break the guard and
confirm the test fails, then restore it. Removing `DEFERRABLE` from the ledger
trigger, removing the dedup constraint, removing the length prefix from the
fingerprint, removing `SELECT FOR UPDATE` from the period close — that last one
failed 3 runs out of 3, which is how I learned it was a genuine race and not
defensive coding. **A test that has never failed is untested.**

**"Anything CI does that's unusual?"**
It refuses to accept a skipped test. Integration tests skip when no database is
reachable, which is right locally and means a green run can prove nothing. CI
sets an environment variable that turns that skip into a failure. A broken
service container would otherwise produce a *passing* build with everything
quietly skipped — strictly worse than a red one, because the badge then claims
the invariants hold when nothing checked them.

Also: **CI applies no migrations.** The test harness replays them itself, so the
tests run against what `migrations/` describes rather than whatever CI set up. A
separate step round-trips them, because "the schema is right" and "the
migrations are reversible" are different claims.

---

## What I'd do differently

Have two or three ready. Volunteering a real weakness lands better than
defending everything.

- **Store the response, not just the id.** Stripe replays the *original response
  body* on a retry. Ingest returns a fresh offset each time, which is honest but
  weaker.
- **Plan versioning.** There's no answer to "what plan produced this invoice",
  and late usage is billed at the *next* period's prices — so if prices changed
  in between, it's charged at the newer rate. That's wrong and undocumented to
  the customer.
- **The type system doesn't prevent the one mistake that matters.** A durability
  policy that acknowledges before syncing is still reachable; only a comment
  stops someone using it behind a `202`.
- **Chaos only injects faults the system is designed to survive.** Nothing yet
  injects one it should legitimately fail on — disk full during a segment roll,
  a torn record from power loss.

## Deliberately not built

Know the boundary and say it confidently: no auth, no UI, no payment
processing, no currency conversion or tax, no replication. The single-node log
is a choice, and it's the reason the fsync tradeoff is forced rather than free.

---

## Numbers worth remembering

| | |
|---|---|
| fsync per append | ~312/sec — and **flat under concurrency** |
| Group commit, 64 writers | ~2,211/sec, same guarantee, 7.2× |
| No fsync | ~76,000/sec, not durable |
| Sparse index | 71 entries for 4,000 records |
| Read path fix | 86 µs → 33 µs by buffering the scan |
| Money scale | 6 decimal places, `int64` micro-units |
| Tests | 231, CI green under `-race` |
