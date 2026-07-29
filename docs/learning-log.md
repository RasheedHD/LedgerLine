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
