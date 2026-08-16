-- Reverses 000005_dead_letters.up.sql.
--
-- Worth being clear about what this destroys. A dead letter holds the only
-- surviving copy of an event the system accepted, acknowledged, and then could
-- not apply -- the broker log has the bytes too, but nothing else records that
-- it FAILED or why. Dropping this table converts every one of those into
-- silently lost usage, which is exactly the state invariant I3 exists to
-- prevent.
DROP TABLE IF EXISTS dead_letters;
