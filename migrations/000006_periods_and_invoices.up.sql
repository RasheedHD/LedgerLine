-- Billing periods and invoices. See ADR-0015.

CREATE TABLE billing_periods (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id TEXT NOT NULL,

    -- Stable label for the window: "2026-08". Part of the ledger idempotency
    -- key, so it must not change for a given period.
    label TEXT NOT NULL,

    -- Half-open [starts_at, ends_at). Consecutive periods then neither overlap
    -- nor leave a gap: an event at exactly midnight belongs to one period,
    -- not both and not neither.
    starts_at TIMESTAMPTZ NOT NULL,
    ends_at TIMESTAMPTZ NOT NULL,

    -- Only two states, deliberately. A 'closing' state is what you need when
    -- closing is long-running and observable half-done; here it is a single
    -- transaction, so a period is open until it commits and closed
    -- afterwards, with no instant in between that anyone can see.
    state TEXT NOT NULL CHECK (state IN ('open', 'closed')),

    closed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL,

    CONSTRAINT billing_periods_tenant_label_unique UNIQUE (tenant_id, label),
    CONSTRAINT billing_periods_ordered CHECK (ends_at > starts_at),

    -- A closed period must record when, and an open one must not pretend to.
    CONSTRAINT billing_periods_closed_at_matches_state
        CHECK ((state = 'closed') = (closed_at IS NOT NULL))
);

CREATE INDEX billing_periods_tenant_state_idx ON billing_periods (tenant_id, state);

CREATE TABLE invoices (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- One invoice per period. The unique constraint is what makes closing
    -- idempotent: a second attempt cannot produce a second invoice.
    period_id BIGINT NOT NULL UNIQUE REFERENCES billing_periods (id),

    tenant_id TEXT NOT NULL,

    -- Signed micro-units, matching ledger.Amount. Denormalised from the line
    -- items on purpose: an invoice's total is what the customer was told, and
    -- it must not change if the code that sums line items is ever altered.
    total BIGINT NOT NULL,

    -- The ledger transaction recognising this revenue, so an invoice can be
    -- traced to its accounting entry and back.
    ledger_transaction_id BIGINT NOT NULL REFERENCES ledger_transactions (id),

    issued_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX invoices_tenant_idx ON invoices (tenant_id);

CREATE TABLE invoice_line_items (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    invoice_id BIGINT NOT NULL REFERENCES invoices (id),

    meter TEXT NOT NULL,

    -- The quantity billed, at NUMERIC(38,9) to match the events it came from.
    quantity NUMERIC(38, 9) NOT NULL,

    -- Signed micro-units, matching ledger.Amount.
    amount BIGINT NOT NULL,

    CONSTRAINT invoice_line_items_invoice_meter_unique UNIQUE (invoice_id, meter)
);

-- Which invoice billed an event, and NULL for everything not yet billed.
--
-- CRITICAL: this column is what closes D39 and what implements ADR-0001 §5.
--
-- "Unbilled" becomes a queryable state rather than something inferred from
-- dates. A posting run bills every unbilled event up to the period end, so an
-- event that arrives after its own period closed is simply still unbilled and
-- is picked up by the next period's run -- the late-event roll-forward falls
-- out of the model instead of needing a special case.
ALTER TABLE events ADD COLUMN invoice_id BIGINT REFERENCES invoices (id);

-- The hot query is "what is unbilled for this tenant up to time T". Partial,
-- because billed events are the overwhelming majority and are never the answer.
CREATE INDEX events_unbilled_idx
    ON events (tenant_id, occurred_at)
    WHERE invoice_id IS NULL;

-- CRITICAL: INVARIANT I4 -- closed invoices are immutable -- enforced by the
-- database rather than by convention.
--
-- Everything downstream of an invoice acted on its number already: revenue
-- recognition, tax, the customer's own books. An invoice that can change after
-- the fact makes all of that unreliable, and no amount of care in application
-- code can promise it will not happen -- a migration or a repair script would
-- bypass it.
CREATE FUNCTION invoices_are_immutable() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'invoice % is closed and cannot be % (invariant I4)',
        COALESCE(OLD.id, NEW.id), lower(TG_OP);
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER invoices_immutable
    BEFORE UPDATE OR DELETE ON invoices
    FOR EACH ROW EXECUTE FUNCTION invoices_are_immutable();

CREATE TRIGGER invoice_line_items_immutable
    BEFORE UPDATE OR DELETE ON invoice_line_items
    FOR EACH ROW EXECUTE FUNCTION invoices_are_immutable();

-- An event may be billed once. Re-stamping it onto a second invoice would bill
-- the same usage twice while every individual record still looked consistent.
CREATE FUNCTION events_billed_once() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.invoice_id IS NOT NULL AND NEW.invoice_id IS DISTINCT FROM OLD.invoice_id THEN
        RAISE EXCEPTION 'event % is already billed on invoice %, refusing to move it to %',
            OLD.id, OLD.invoice_id, NEW.invoice_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER events_billed_once_trigger
    BEFORE UPDATE ON events
    FOR EACH ROW EXECUTE FUNCTION events_billed_once();
