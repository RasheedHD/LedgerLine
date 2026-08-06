-- Where each consumer has got to in the broker log.
--
-- CRITICAL: this table lives in the same database as `events` on purpose.
--
-- The consumer advances its offset in the SAME TRANSACTION as the insert it is
-- advancing past. That is what makes processing exactly-once rather than
-- at-least-once: the two either commit together or roll back together, so
-- there is no window where an event has been stored but its offset not
-- recorded, or the reverse.
--
-- Keeping the offset anywhere else -- a file, another database -- reintroduces
-- that window, because two stores cannot be committed atomically without a
-- distributed transaction. See ADR-0009.

CREATE TABLE consumer_offsets (
    -- Named rather than anonymous, so a second consumer (an analytics reader,
    -- a rebuild) can track its own position independently.
    consumer TEXT PRIMARY KEY,

    -- The offset to read NEXT, not the last one processed. Storing "next"
    -- makes the empty case (nothing consumed yet) a plain zero rather than a
    -- sentinel meaning "before the beginning".
    next_offset BIGINT NOT NULL,

    updated_at TIMESTAMPTZ NOT NULL,

    -- Offsets only ever move forward. A negative value would mean the counter
    -- wrapped or was corrupted, and it is cheaper to refuse it here than to
    -- work out later why a consumer replayed the whole log.
    CONSTRAINT consumer_offsets_next_offset_non_negative CHECK (next_offset >= 0)
);
