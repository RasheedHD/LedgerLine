-- Reverses 000002_add_payload_fingerprint.up.sql.
--
-- Dropping the column loses every fingerprint irrecoverably: they are hashes,
-- so re-deriving them means re-reading the original payloads, which we do not
-- keep. Running this down migration means giving up reuse detection for all
-- existing rows, not merely for new ones.
--
-- IF EXISTS so a down run against a partially-applied state does not fail and
-- leave schema_migrations dirty.
ALTER TABLE events
    DROP COLUMN IF EXISTS payload_fingerprint;
