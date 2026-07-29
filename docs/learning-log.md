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
