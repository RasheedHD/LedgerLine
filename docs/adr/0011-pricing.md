# ADR-0011: Pricing and the meter registry

- **Status:** Accepted
- **Date:** 2026-08-03
- **Deciders:** Rasheed
- **Related:** [ADR-0010](0010-double-entry-ledger.md), [ADR-0001](0001-event-schema.md)
- **Resolves:** PLAN.md debt item D12

## Context

The ledger could record money but nothing produced any. Pricing is the step
between a quantity of usage and an amount, and it is the part of a billing
system a customer will argue with — so it has to be reproducible on demand,
months later, to the exact figure.

## Decision

### Rating is a pure function

`Rate(usages, plan, registry) → line items`. No clock, no database, no
randomness, no dependence on map iteration order.

This is what invariant I5 rests on, and it is also what makes a disputed
invoice reproducible. Two consequences that look like awkward style choices and
are not:

- **`Plan.Prices` is a slice, not a map keyed by meter.** Go randomises map
  iteration order deliberately. Rating that walked a map would emit line items
  in a different order every run, and I5 asks for a byte-identical result.
- **Output is sorted by meter name.** So input order cannot leak into output.
  There is a test that shuffles the input fifty times and asserts the result
  never changes.

A map *is* used inside `Rate` as an accumulator, but its keys are extracted and
sorted before anything depends on their order.

### Usage is aggregated before pricing, not priced per event

All usage for a meter is summed, then priced once.

This is not an optimisation. With tiered prices the two give **different
answers**: pricing a thousand events individually puts every one of them in the
first tier and never reaches a discount the customer is entitled to. There is a
test that sends 1500 units as fifteen events of 100 and asserts the result is
$10.50 (tiered) rather than $15.00 (priced separately).

It also confines rounding to one operation per meter rather than one per event,
which is where the error would otherwise accumulate.

### Three models

- **Flat** — every unit at one price.
- **Graduated** — the quantity is split across tiers, each portion charged at
  its own tier's rate.
- **Volume** — the *total* selects a single tier and every unit is charged at
  that rate.

Both tiered models exist because they are genuinely different products.
Crossing a boundary under volume pricing makes the earlier units cheaper too,
so the bill can go **down** as usage goes up. There is a test asserting the two
models disagree across a boundary — if they ever agreed, one of them would not
be implemented.

Tier bounds are **inclusive**, and every boundary is tested at exactly, one
below, and one above.

`UpTo` is a `*Quantity` where nil means unbounded, rather than a sentinel like
`-1` or `MaxInt64`. A sentinel is indistinguishable from a real bound at the
call site, and confusing the two would silently mis-price everything above it.

`validateTiers` refuses a bounded last tier, because usage above the final
bound would otherwise have **no price at all** and be silently free — the
undercounting failure I3 cares most about.

### The meter registry

Usage for an unregistered meter is an error, never a silent zero. Closes D12.

Without it, a client sending `api_call` instead of `api_calls` is billed
nothing and nobody finds out. A registered meter with no price in the plan is
also an error, for the same reason: usage nobody has priced is revenue quietly
going missing.

### Arithmetic

`Quantity` is an `int64` count of nano-units, matching the `NUMERIC(38,9)`
column events are stored in. `ledger.Amount` is micro-units. Their product
carries fifteen decimal places, which overflows `int64` at unremarkable
magnitudes, so the multiplication is done in `math/big` and the result checked
back into `int64`.

Rounding is **half-up on the absolute value**, applied once per meter. Half-up
is what people expect money to do. Banker's rounding reduces long-run bias and
surprises everyone who checks the arithmetic by hand; for an invoice a customer
may query, predictability wins.

## I6, enforced by a test rather than by review

`TestNoFloatingPointOnTheMoneyPath` walks every non-test file under `billing/`
and fails if any uses `float64` or `float32`.

The first version searched the file bytes and **failed immediately** — on three
comments explaining why float is avoided. Those comments are exactly the lines
worth keeping, so the test was wrong, not the code. It now parses each file and
inspects the AST, where comments are simply absent unless asked for, which
draws the line in the right place with no special-casing.

Verified by planting a real `float64` and watching it fail, then removing it.

## Consequences

- `Quantity` at `int64` nanos holds about 9.2 billion whole units, which is
  **narrower than the `NUMERIC(38,9)` column it comes from**. A quantity that
  passes ingest can therefore fail to price. Recorded as **D33**; ingest's own
  bound is 29 integer digits, so the two disagree.
- Nothing yet turns priced line items into ledger transactions. Rating produces
  amounts; posting them is a separate step with its own chart of accounts, and
  it is not built.
- Plans and meters are constructed in memory by the caller. There is no
  persistence, no versioning, and therefore no answer yet to "what did this
  plan look like when that invoice was cut" — which is a question anyone
  disputing an invoice will eventually ask. **D34.**
- Flat fees are charged per tier reached under graduated pricing. That is one
  reasonable reading of a tier flat fee; charging it once per invoice is
  another. Untested against a real product requirement.

## Open questions

- Minimum commitments, credits, prepaid balances, and proration are all
  unmodelled.
- Currency is absent, consistent with ADR-0010 and PLAN.md's non-goals.
- Whether `Rate` should return the tier breakdown alongside the total. An
  invoice line that says only "$145" invites the question the breakdown would
  answer.
