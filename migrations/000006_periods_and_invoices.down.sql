-- Reverses 000006_periods_and_invoices.up.sql.
--
-- Destroys every invoice ever issued. The ledger entries survive, so the
-- accounting still balances, but the record of what each customer was actually
-- told they owed is gone -- and so is the mark showing which events were
-- billed, which means the next posting run would bill all of them again.
--
-- Order matters: triggers and the events column go before the tables they
-- reference, and line items before invoices before periods.
DROP TRIGGER IF EXISTS events_billed_once_trigger ON events;
DROP FUNCTION IF EXISTS events_billed_once();

ALTER TABLE IF EXISTS events DROP COLUMN IF EXISTS invoice_id;

DROP TRIGGER IF EXISTS invoice_line_items_immutable ON invoice_line_items;
DROP TRIGGER IF EXISTS invoices_immutable ON invoices;
DROP FUNCTION IF EXISTS invoices_are_immutable();

DROP TABLE IF EXISTS invoice_line_items;
DROP TABLE IF EXISTS invoices;
DROP TABLE IF EXISTS billing_periods;
