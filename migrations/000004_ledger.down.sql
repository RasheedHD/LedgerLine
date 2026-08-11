-- Reverses 000004_ledger.up.sql.
--
-- Dropping these tables destroys the accounting record outright. Unlike the
-- events table, which the broker log can be replayed into, nothing else in the
-- system holds this: the ledger is derived from events but the derivation is
-- not free, and any manual adjustments posted directly have no other source.
--
-- Order matters. Postings reference both other tables, so it goes first. The
-- trigger and its function belong to ledger_postings and go with it, but the
-- function is dropped explicitly because a function outlives the trigger that
-- used it.
DROP TABLE IF EXISTS ledger_postings;
DROP FUNCTION IF EXISTS ledger_assert_balanced();
DROP TABLE IF EXISTS ledger_transactions;
DROP TABLE IF EXISTS ledger_accounts;
