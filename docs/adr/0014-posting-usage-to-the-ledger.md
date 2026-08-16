# ADR-0014: Posting usage to the ledger

- **Status:** Accepted
- **Date:** 2026-08-11
- **Deciders:** Rasheed
- **Related:** [ADR-0010](0010-double-entry-ledger.md), [ADR-0011](0011-pricing.md)
- **Resolves:** PLAN.md debt item D35

## Context

Pricing could turn quantities into money and the ledger could record money, but
nothing joined them. Two good libraries, no billing system.

## Decision

**`billing/posting` reads a tenant's usage for a period, prices it, and posts
one balanced ledger transaction.**

### The chart of accounts is generated, not configured

- `receivable:<tenant_id>` — asset, one per tenant
- `revenue:<meter>` — revenue, one per meter

An account per tenant on the asset side, because "what does this customer owe"
cannot be answered from a single pooled receivable. An account per meter on the
revenue side, so revenue is reportable by product line rather than as one
undifferentiated number.

Neither set can be known in advance — tenants and meters arrive over time — so
names are derived deterministically and accounts created on demand via
`EnsureAccount`.

**The prefixes are load-bearing.** Without them a tenant called `api_calls`
would share an account with the revenue for the `api_calls` meter, and two
unrelated figures would accumulate in one place. There is a test asserting the
namespaces are disjoint.

`EnsureAccount` **refuses** to return an account that exists with a different
kind. Adopting it would flip the sign every report reads it with while the
existing postings stayed as they were.

### The entry

For each priced line item: **debit the tenant's receivable, credit the meter's
revenue.** That is revenue recognition — the customer now owes us (an asset
increases) and we have earned it (revenue increases). One transaction per
tenant per period, with one transfer per meter.

### The idempotency key is derived

`usage:<tenant>:<period-label>`.

Same tenant and period always produce the same key, so a run repeated after a
crash, a retry, or an operator running it twice records the usage once. **A
random key would post the same revenue again on every run, and the ledger would
balance perfectly while being wrong** — which is the failure double-entry alone
cannot catch, because both sides of a duplicate entry are equally wrong.

This is invariant I2 at the ledger boundary, behind the dedup already done in
the consumer.

### Periods are half-open, and placed by event time

`[Start, End)`. Consecutive periods neither overlap nor leave a gap: an event
at exactly midnight belongs to one period, not both and not neither. There is a
test with events at one nanosecond before the start, exactly at the start, at
the last instant, and exactly at the end.

Usage is selected by **`occurred_at`, not `received_at`**. Which period usage
belongs to is decided by when it happened, not when we heard about it. That is
what ADR-0001's two clocks are for, and it is what makes a late event bill
against the period it actually occurred in — tested with an event that arrived
72 hours after its period ended.

`Period` is deliberately just a labelled time range. The open/closing/closed
state machine invariant I4 needs is Phase 6 work, and inventing half of it here
would have to be undone.

### Aggregation happens in SQL

`SUM(quantity) GROUP BY meter`. Postgres sums `NUMERIC` exactly, so nothing is
lost, and a busy tenant's millions of monthly events never cross the wire.

The result feeds `pricing.Rate`, which is what makes tiering correct: the
period **total** selects the tier. There is a test posting 1500 calls as fifteen
events of 100 and asserting $10.50 (tiered on the total) rather than $15.00
(each event alone in the first tier).

### Zero posts nothing

Usage that prices to zero — a free tier, a zero-rate meter — produces no
transaction. `ledger.Transfer` requires a positive amount, and rightly: a zero
posting balances trivially while recording nothing, hiding whatever produced
it. Zero-priced usage is expected, not an error, so it returns
`ErrNothingToPost` rather than failing.

## Verified end to end

`TestFullPipelineFromHTTPToLedger` runs the entire system with no component
faked: 100 distinct events plus 40 retries posted over HTTP, through the broker
log, through the consumer, into `events`, priced, and posted.

- 140 records on the log
- 100 inserted, 40 duplicates, `Stats.Accounted()` true
- Invoice total **$1.00**, not $1.40
- Receivable balance $1.00, trial balance exactly 0
- Re-running the billing job reports `AlreadyPosted` and changes nothing

That single test is the project's thesis reduced to one assertion.

## Consequences

- Nothing records **which events** a posting run included. If usage arrives for
  a period after it has been posted, re-running produces `AlreadyPosted` and
  the new usage is never billed — silently. That is the sharpest open problem
  in the system and it is what Phase 6's period state machine has to solve
  (**D39**).
- Plans are passed in by the caller and not persisted, so there is still no
  answer to "what plan produced this invoice" (D34, unchanged).
- A posting run holds no lock. Two concurrent runs for the same tenant and
  period race, and the loser gets `AlreadyPosted` — correct, but by accident of
  the unique constraint rather than by design.
- Revenue accounts are global rather than per tenant. Revenue by meter is
  reportable; revenue by meter *per tenant* is not, without summing the
  postings a different way.

## Open questions

- Should the transaction reference the events it came from, so an invoice line
  can be expanded into the usage behind it? Needed for a customer dispute, and
  it is the natural shape for D39's fix.
- Credits, refunds, and adjustments have no posting rule yet. They are the
  reason `ledger` allows arbitrary transfers rather than only this one shape.
