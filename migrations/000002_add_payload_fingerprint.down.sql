-- Reverses 000002_add_payload_fingerprint.up.sql.
--
-- Dropping the column loses every fingerprint irrecoverably: they are hashes,
-- so re-deriving them means re-reading the original payloads, which we do not
-- keep. Running this down migration means giving up reuse detection for all
-- existing rows, not merely for new ones.
--
-- IF EXISTS twice, and both are needed.
--
-- `DROP COLUMN IF EXISTS` only guards the column. Without `ALTER TABLE IF
-- EXISTS` as well, running this against a database where `events` has already
-- been dropped fails with 42P01 and leaves schema_migrations dirty -- which is
-- exactly the state a down migration is supposed to be able to clean up.
ALTER TABLE IF EXISTS events
    DROP COLUMN IF EXISTS payload_fingerprint;
