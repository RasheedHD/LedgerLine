-- ADR-0005: detect an idempotency key reused for different usage.
--
-- Without this, a client that recycles a key for a genuinely different event
-- has that event silently discarded and receives a 202 saying it was stored.
-- We lose billable usage and report success, which is the worst combination
-- available: the customer is undercharged and nobody finds out.

ALTER TABLE events
    ADD COLUMN payload_fingerprint BYTEA;

-- Deliberately nullable rather than NOT NULL.
--
-- Rows written before this migration have no fingerprint and there is no
-- honest way to compute one for them -- the hash is defined over a
-- canonicalisation the application performs, not something SQL can reproduce.
-- A backfilled placeholder would be worse than NULL: two rows sharing a
-- sentinel value would compare equal and silently claim their payloads match.
--
-- NULL therefore means "predates fingerprinting, reuse cannot be detected for
-- this row", and the application treats it that way rather than guessing.
-- Every row written from now on carries one.
COMMENT ON COLUMN events.payload_fingerprint IS
    'SHA-256 over the billable fields (tenant_id, meter, canonical quantity, occurred_at). NULL means the row predates migration 000002.';

-- No index. This column is only ever read through the existing
-- (tenant_id, idempotency_key) unique lookup, which already located the row.
