# ADR-0015: Billing periods and invoices

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Rasheed
- **Supersedes:** ADR-0014's `billing/posting` package
- **Implements:** [ADR-0001](0001-event-schema.md) §5
- **Resolves:** PLAN.md debt items D3, D39, D40

## Context

Three things had converged on this:

- **I4 — closed invoices are immutable — had no enforcement anywhere**, because
  nothing closed.
- **D39 was a live bug.** `posting.Post` had no record of which events a run
  included, so usage arriving for an already-posted period was silently never
  billed.
- **ADR-0001 §5's late-event roll-forward**, designed in the very first
  session, had never been implemented.

All three are the same missing concept: a period that can be *closed*.

## Decision

### `billing/posting` is deleted, not extended

Its `Post` did not know about invoices, so once invoices exist it would re-bill
events already billed. Keeping it would have meant two billing paths, one of
which double-charges. The chart-of-accounts naming moved into `billing/invoicing`
unchanged; the rest is superseded.

It lasted one commit. That is the right outcome for a stepping stone — it was
useful for proving pricing and the ledger could be joined at all, and the period
model is what that join actually needed.

### Two states, not three

`open` and `closed`. PLAN.md sketched `open → closing → closed`, and `closing`
has been dropped.

A `closing` state earns its place when closing is long-running and observable
half-done. Here it is one transaction: the period is open until it commits and
closed afterwards, with no instant anyone can observe in between. A state that
can never be seen is a state that will eventually be handled wrong.

### The unbilled-event mark is the whole design

`events.invoice_id`, nullable. NULL means unbilled.

This one column does the work of all three problems above:

```sql
WHERE tenant_id = $1 AND invoice_id IS NULL AND occurred_at < $2
```

**There is deliberately no lower bound.** Selecting `[start, end)` would leave
an event that arrived after its own period closed unbilled forever — it belongs
to a window nobody will gather again. Gathering *everything still unbilled* up
to the end of this period means such an event is picked up by the next run.

So ADR-0001 §5's roll-forward is not a special case. It is what the query
already does. The event keeps its original `occurred_at`, is billed in the next
open period, and the line item is flagged `Late` so the invoice can explain
itself.

### The mark and the invoice are written together

`markBilled` uses the same predicate that gathered the usage, in the same
transaction. Anything else and an event arriving between the SELECT and the
UPDATE is either billed without being marked — and so billed again next period
— or marked without being billed, which loses it silently. Inside one
transaction the snapshot is stable, so the two sets are identical.

The ledger posting, the invoice, the line items, the event marks, and the state
change are all one transaction. An invoice with no ledger entry behind it, or
the reverse, is worse than neither.

### `SELECT ... FOR UPDATE` on the period

Closes D40. Two concurrent closes previously both read `state='open'`, both
gathered the same events, and one lost on a constraint somewhere downstream —
correct by accident rather than by design.

The row lock serialises them properly. **Mutation-tested: removing `FOR UPDATE`
makes `TestConcurrentClosesProduceOneInvoice` fail on 3 runs out of 3**, so this
was a genuine race, not a theoretical one.

### I4 is enforced by the database

`BEFORE UPDATE OR DELETE` triggers on `invoices` and `invoice_line_items` that
raise unconditionally. The Go code never updates one, but a migration or repair
script would bypass that, so the rule lives in the schema where nothing can go
around it.

A second trigger refuses to move an event from one invoice to another. That
would bill the same usage twice while every individual row still looked
consistent.

Both mutation-tested: removing the triggers makes the tests fail.

### The invoice total is denormalised

`invoices.total` is stored, not summed from line items on read. An invoice's
total is what the customer was told, and it must not change if the code that
sums line items is ever altered. Storing it makes the number a fact rather than
a derivation.

## Consequences

- Usage arriving after a period closes is billed in the **next** period, at
  that period's prices. If prices changed between them, late usage is charged
  at the newer rate. Arguably wrong, definitely undocumented to the customer,
  and it needs plan versioning (D34) before it can be fixed properly.
- A tenant with no open period accumulates unbilled events indefinitely. There
  is nothing that creates periods on a schedule (**D41**).
- `Close` bills everything unbilled up to `period.End`, including usage older
  than the previous period. That is intended — it is the roll-forward — but it
  means a very late event can appear on an invoice months after it occurred,
  flagged `Late` and nothing more.
- Invoices cannot be voided or credited. The immutability triggers make that
  impossible by design; a correction has to be a new ledger entry, which is
  the right shape but has no API yet.

## Open questions

- Should an invoice record which *events* it billed, rather than only the
  reverse pointer? The reverse is enough to compute it, but a customer dispute
  wants the forward list.
- Late usage currently only gets a boolean. A line item that said "of which
  $0.50 occurred in July" would be far more explainable.
- Nothing decides when a period should close. A scheduler, or closing on first
  read after the end date, are both plausible and neither is built.
