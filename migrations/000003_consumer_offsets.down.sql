-- Reverses 000003_consumer_offsets.up.sql.
--
-- Dropping this table loses every consumer's position. On the next start each
-- consumer would restart from offset 0 and replay the entire log. That is
-- survivable -- the unique constraint on events absorbs the replay without
-- double-billing anyone, which is invariant I2 doing its job -- but it is a
-- full reprocessing run, not a no-op.
DROP TABLE IF EXISTS consumer_offsets;
