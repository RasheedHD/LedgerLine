-- Records the consumer accepted off the log but could not apply. See ADR-0013.
--
-- Invariant I3 says no accepted usage is silently lost: anything that received
-- a 202 either reaches the ledger or lands in a dead letter with a recorded
-- reason. Until now the second half was a log line, which is not somewhere a
-- human can act on later -- and since ADR-0012 removed ingest's ability to
-- report key reuse to the client, that log line was the ONLY trace that usage
-- had been dropped.

CREATE TABLE dead_letters (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- Which consumer gave up on it, matching consumer_offsets.consumer.
    consumer TEXT NOT NULL,

    -- Where on the log the record sits. Named log_offset rather than offset
    -- because OFFSET is a reserved word in SQL and would need quoting forever.
    log_offset BIGINT NOT NULL,

    reason TEXT NOT NULL CHECK (reason IN ('undecodable_record', 'idempotency_key_reuse')),
    detail TEXT NOT NULL,

    -- Nullable: an undecodable record has no readable tenant or key by
    -- definition. Storing them when known is what makes a dead letter
    -- searchable by the customer who complains.
    tenant_id TEXT,
    idempotency_key TEXT,

    -- CRITICAL: the raw log record, verbatim.
    --
    -- Without it a dead letter says only "something failed at offset 412",
    -- which is not actionable. With it the event can be inspected, explained to
    -- the customer, and replayed by hand once the cause is fixed. This is the
    -- difference between I3 being satisfied and merely being claimed.
    record BYTEA NOT NULL,

    created_at TIMESTAMPTZ NOT NULL,

    -- Set when a human has dealt with it. Nullable, and nothing in the system
    -- writes it yet -- it exists so that "outstanding dead letters" is a query
    -- rather than a spreadsheet.
    resolved_at TIMESTAMPTZ,

    -- CRITICAL: makes replay idempotent.
    --
    -- Reprocessing the log from offset 0 re-encounters every failed record.
    -- Without this constraint each replay would append another copy, so the
    -- dead-letter table would grow on every rebuild and invariant I5 -- replay
    -- produces the same state -- would not hold.
    CONSTRAINT dead_letters_consumer_offset_unique UNIQUE (consumer, log_offset)
);

-- The operational query is "what is outstanding for this consumer", so the
-- partial index covers exactly that and skips rows already dealt with.
CREATE INDEX dead_letters_unresolved_idx
    ON dead_letters (consumer, created_at)
    WHERE resolved_at IS NULL;

-- Supports answering "did anything of mine get dropped?" for a customer who
-- asks. Partial for the same reason as above.
CREATE INDEX dead_letters_tenant_idx
    ON dead_letters (tenant_id)
    WHERE tenant_id IS NOT NULL;
