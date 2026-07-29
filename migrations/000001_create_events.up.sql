-- ADR-0001: the UsageEvent schema.
--
-- This table is the durable landing zone for usage in Postgres. The broker's
-- log is what makes an event survive a crash; by the time a row lands here it
-- is already durable upstream. Nothing in this table is ever deleted --
-- see the note on the unique constraint below.

CREATE TABLE events (
    -- Surrogate key. Narrow (8 bytes), so secondary indexes stay small, and
    -- monotonically increasing, which gives us an insertion order that lines
    -- up with the broker's offset concept. GENERATED ALWAYS means the
    -- application cannot supply this value even by accident.
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    tenant_id TEXT NOT NULL,
    meter     TEXT NOT NULL,

    -- ADR-0001 section 3. NUMERIC, never float: binary floating point cannot
    -- represent most decimal fractions exactly, so summing millions of these
    -- would drift and the ledger would fail to balance.
    -- (38, 9) bounds it so a malformed client cannot insert a 900-digit
    -- quantity that only becomes a problem at invoice time.
    quantity NUMERIC(38, 9) NOT NULL,

    -- ADR-0001 section 1. Two timestamps, not one.
    --
    -- occurred_at is EVENT time: when the usage actually happened. It decides
    -- which billing period the event belongs to. Client-supplied, which is
    -- also why a client with a bad clock is listed as an open question in
    -- the ADR -- there is no clamp on this yet.
    occurred_at TIMESTAMPTZ NOT NULL,

    -- received_at is INGEST time: when we took durable custody. It decides
    -- what we knew and when.
    --
    -- Deliberately no DEFAULT now(). The broker owns this value and stamps it
    -- at durable write, which happens before the row reaches Postgres. A
    -- default here would look like a safety net but would actually mask the
    -- bug where the application forgot to set it -- the insert would succeed
    -- with a plausible-looking timestamp and every lateness calculation
    -- downstream would be quietly wrong.
    received_at TIMESTAMPTZ NOT NULL,

    -- ADR-0001 section 2. Client-generated, reused verbatim on retry.
    idempotency_key TEXT NOT NULL,

    -- CRITICAL: this is where dedup correctness actually lives.
    --
    -- Scoped per-tenant, because two tenants independently generating the
    -- same UUID must not collide -- and because an unscoped constraint would
    -- let one tenant probe another's key space by observing conflicts.
    --
    -- Note this is unbounded in time, which is STRICTER than the 7-day window
    -- in ADR-0001 section 4. That was the deliberate choice: rows here are
    -- billing records and are never dropped on a timer. The bounded,
    -- day-partitioned structure the ADR describes is a separate dedup table
    -- in a later migration; this constraint is the backstop underneath it.
    CONSTRAINT events_tenant_idempotency_key_unique
        UNIQUE (tenant_id, idempotency_key)
);

-- Not specified in ADR-0001 -- added because the core invoicing query is
-- "all usage for tenant X in period Y", and without this it is a sequential
-- scan of the whole table. occurred_at (event time) rather than received_at,
-- because periods are defined in event time.
-- Drop this line if you would rather add indexes only once you have a real
-- query plan to justify them.
CREATE INDEX events_tenant_occurred_at_idx ON events (tenant_id, occurred_at);
