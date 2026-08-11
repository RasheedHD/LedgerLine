-- Double-entry ledger. See ADR-0010.

CREATE TABLE ledger_accounts (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,

    -- The five classical account kinds. A CHECK rather than an enum type
    -- because adding a value to a Postgres enum is a schema change with
    -- awkward transactional behaviour, and this list has been stable since
    -- the fifteenth century.
    kind TEXT NOT NULL CHECK (kind IN ('asset', 'liability', 'revenue', 'expense', 'equity')),

    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE ledger_transactions (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- Posting the same transaction twice is a no-op. Invariant I2 at the
    -- ledger boundary: ingest already deduplicates, and this is the backstop
    -- behind it, because the ledger is where being wrong costs money.
    idempotency_key TEXT NOT NULL UNIQUE,

    -- Event time. Decides which period the transaction belongs to, exactly as
    -- occurred_at does for a usage event.
    occurred_at TIMESTAMPTZ NOT NULL,

    description TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE ledger_postings (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,

    -- ON DELETE CASCADE is deliberately absent. Ledger history is not
    -- deletable; a mistake is corrected by posting a reversing entry, which
    -- leaves both the error and the correction visible. Deleting the original
    -- would erase the evidence that anything happened.
    transaction_id BIGINT NOT NULL REFERENCES ledger_transactions (id),
    account_id BIGINT NOT NULL REFERENCES ledger_accounts (id),

    -- Signed micro-units: positive is a debit, negative is a credit.
    --
    -- BIGINT, never NUMERIC or a float. An integer count of a fixed unit is
    -- exact by construction, and the scale (6 places) is fixed in Go's
    -- ledger.Scale. Storing money as a float would reintroduce exactly the
    -- accumulation error double-entry exists to catch.
    --
    -- Signing the amount makes "this transaction balances" identical to
    -- "these rows sum to zero", which is far easier to assert than comparing
    -- two separately accumulated totals.
    amount BIGINT NOT NULL CHECK (amount <> 0)
);

CREATE INDEX ledger_postings_transaction_idx ON ledger_postings (transaction_id);

-- Balances are computed by summing an account's postings, so this index is
-- what keeps that from being a sequential scan of the whole ledger.
CREATE INDEX ledger_postings_account_idx ON ledger_postings (account_id);

-- CRITICAL: the database enforces balance independently of the application.
--
-- The Go API makes an unbalanced transaction impossible to express, which is
-- the primary defence. This is the second one, and it matters because the Go
-- API is not the only thing that can ever write to these tables -- a
-- migration, a repair script, or a future service all bypass it.
CREATE FUNCTION ledger_assert_balanced() RETURNS TRIGGER AS $$
DECLARE
    total BIGINT;
BEGIN
    SELECT COALESCE(SUM(amount), 0) INTO total
    FROM ledger_postings
    WHERE transaction_id = NEW.transaction_id;

    IF total <> 0 THEN
        RAISE EXCEPTION 'ledger transaction % does not balance: postings sum to %',
            NEW.transaction_id, total;
    END IF;

    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- DEFERRABLE INITIALLY DEFERRED is the load-bearing part.
--
-- Postings are inserted one row at a time, so a transaction is unbalanced
-- after its first row and only becomes balanced once the last one lands. An
-- immediate check would reject every legitimate entry. Deferring it to COMMIT
-- means the rule is evaluated when the whole entry is present, which is the
-- only moment "balanced" is even a meaningful question.
--
-- CONSTRAINT TRIGGER rather than a plain trigger because only constraint
-- triggers can be deferred.
CREATE CONSTRAINT TRIGGER ledger_postings_balanced
    AFTER INSERT ON ledger_postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION ledger_assert_balanced();
