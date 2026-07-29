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
