# ADR-0010: The double-entry ledger

- **Status:** Accepted
- **Date:** 2026-08-03
- **Deciders:** Rasheed
- **Related:** [ADR-0001](0001-event-schema.md)

## Context

Invariant I1 — money is conserved — had no enforcement anywhere in the project.
Nothing computed or recorded an amount, so the strongest guarantee in PLAN.md
was aspirational.

Double-entry is not bookkeeping ceremony. It is a **redundancy check**: every
amount is recorded twice, in opposite directions, so a single arithmetic or
logic error makes the books visibly fail to balance instead of quietly
producing a wrong number. That is the entire reason to use it over a balance
column.

## Decision

### Amounts are integers, at six decimal places

`Amount` is an `int64` count of micro-units. `Scale = 6`.

Six places, not two. Usage billing prices things below a cent — $0.0001 per API
call is ordinary — so a ledger denominated in cents rounds every individual
posting to zero and bills nothing. Rounding to a currency's minor unit is a
**presentation** decision belonging at invoice time, not a storage decision that
destroys the numbers on the way in. Google Ads and most usage-billing systems
land on the same scale, usually called micros.

An integer count of a fixed unit is exact by construction, which is the same
argument ADR-0001 §3 made for `NUMERIC` over float, applied where it matters
most. There is a test that adds $0.0001 a hundred thousand times and asserts the
result is exactly $10.

`ParseAmount` **refuses** more precision than it stores rather than rounding it.
Silently rounding money at the boundary of the system is precisely where nobody
would ever notice.

### Unbalanced transactions are impossible to express

The API accepts `Transfer{Debit, Credit, Amount}` — never a bare posting. A
transfer names both sides and one amount, so it necessarily produces one debit
and one credit of equal size. A `Transaction` is built only from transfers, so
it balances **by construction** rather than by a validation step some future
code path might skip.

The postings slice is unexported and `Postings()` returns a copy, so a caller
cannot rewrite an amount after construction and unbalance a transaction the type
promised was balanced. There is a test that tries.

Multi-legged entries still work: one debit against two credits is two transfers
sharing a debit account. Worth checking, because a model that could not express
that would have bought the construction guarantee at too high a price.

### Postings are signed, positive for debit

That makes "this transaction balances" the identical statement to "these
postings sum to zero" — a far easier property to assert, in Go, in SQL, and in
a test, than accumulating two totals separately and comparing them.

### The database enforces balance independently

A deferred constraint trigger checks that each transaction's postings sum to
zero.

The Go API is the primary defence, but it is not the only thing that can write
to these tables: a migration, a repair script, or a future service all bypass
it. The trigger is what makes the guarantee a property of the *data* rather than
of one package.

**`DEFERRABLE INITIALLY DEFERRED` is load-bearing.** Postings are inserted one
row at a time, so a transaction is unbalanced after its first row and only
becomes balanced when the last one lands. An immediate check rejects every
legitimate entry. Deferring to `COMMIT` means the rule is evaluated when the
whole entry is present, which is the only moment "balanced" is even a meaningful
question.

This was verified by removing `DEFERRABLE` and re-running: every normal post
failed with *"ledger transaction 1 does not balance: postings sum to 12500000"*
after the first insert. And by neutering the check itself, which made the
rejection test fail — so the test is genuinely exercising the trigger and not
some incidental constraint.

### Balances are summed from the journal

No stored running total. There is nothing to drift out of step with the
journal, and the journal is the record of what actually happened. `TrialBalance`
sums every posting in the ledger and must always be exactly zero — invariant I1
as a single query.

## What the property test actually proves

`TestTransactionsAlwaysBalance` generates 2000 random transactions of up to
eight transfers between random accounts and asserts each sums to zero. A handful
of hand-picked examples would prove the examples work; generating them proves
the *construction* works.

`TestTrialBalanceIsAlwaysZero` posts 200 random transactions and re-checks the
whole-ledger sum after every one.

## A wrong assumption, corrected

The first version of the overflow test asserted that several maximum-sized
transfers would overflow `int64` and be refused. It failed, and the test was
wrong rather than the code.

Postings are appended as `+a, -a, +b, -b`, so the running total alternates
between one transfer's amount and zero. It never exceeds the largest individual
transfer, which is a valid `Amount` by definition. **Overflow is structurally
impossible in `Balance`**, not merely unlikely.

The checked arithmetic in `Add` still earns its place, for callers accumulating
in some other order — per account, for instance — where a total genuinely can
run past `int64`. Go does not trap overflow, and a wrapped total flips a large
positive balance negative while both sides of the ledger wrap consistently, so
every downstream check passes on nonsense.

## Consequences

- The ledger has no notion of currency. Fine while everything is one currency,
  and PLAN.md lists conversion as a non-goal, but a second currency would need
  the amount and the account to carry one, and mixing them to be refused.
- `Balance` sums the whole history of an account on every call. Correct, and
  linear in postings. A materialised balance is the obvious optimisation and
  the obvious source of drift; it should not be added without a measurement.
- Postgres `SUM(bigint)` returns `numeric`, so a total beyond `int64` fails the
  scan into `Amount` rather than wrapping. Loud, which is right.
- Ledger rows have no `ON DELETE CASCADE` and no delete path. A mistake is
  corrected by posting a reversing entry, which leaves both the error and the
  correction visible. Deleting the original would erase the evidence.

## Open questions

- No period or invoice concept yet, so nothing closes and I4 remains
  unenforced. That is Phase 6.
- Nothing posts to the ledger automatically. Wiring events through pricing into
  transactions is Phase 4 plus the ledger's own posting rules, and the chart of
  accounts is currently whatever a caller creates.
- `Post` re-verifies balance before writing. Redundant given construction, and
  kept because `Post` is the last point before numbers become permanent.
