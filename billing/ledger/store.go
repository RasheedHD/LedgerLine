package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store persists accounts and transactions in Postgres.
type Store struct {
	db *sql.DB
}

// NewStore returns a Store backed by db.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// ErrAccountNotFound means an account id or name has no matching row.
var ErrAccountNotFound = errors.New("ledger: account not found")

const (
	insertAccount = `
INSERT INTO ledger_accounts (name, kind, created_at)
VALUES ($1, $2, $3)
RETURNING id`

	selectAccountByName = `
SELECT id, name, kind FROM ledger_accounts WHERE name = $1`

	// ensureAccount creates an account or returns the existing one's id.
	//
	// DO UPDATE with a no-op SET rather than DO NOTHING, for the same reason as
	// the events insert: DO NOTHING returns no row on conflict, and recovering
	// the id with a follow-up SELECT races -- under READ COMMITTED the
	// conflicting row may belong to a transaction that has not committed yet.
	// DO UPDATE blocks until it resolves and then returns the row.
	ensureAccountSQL = `
INSERT INTO ledger_accounts (name, kind, created_at)
VALUES ($1, $2, $3)
ON CONFLICT (name) DO UPDATE SET name = ledger_accounts.name
RETURNING id, kind`

	insertTransaction = `
INSERT INTO ledger_transactions (idempotency_key, occurred_at, description, created_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (idempotency_key) DO NOTHING
RETURNING id`

	selectTransactionByKey = `
SELECT id FROM ledger_transactions WHERE idempotency_key = $1`

	insertPosting = `
INSERT INTO ledger_postings (transaction_id, account_id, amount)
VALUES ($1, $2, $3)`

	selectAccountBalance = `
SELECT COALESCE(SUM(amount), 0) FROM ledger_postings WHERE account_id = $1`

	selectTrialBalance = `
SELECT COALESCE(SUM(amount), 0) FROM ledger_postings`
)

// CreateAccount adds an account and returns its id.
func (s *Store) CreateAccount(ctx context.Context, name string, kind AccountKind) (AccountID, error) {
	if name == "" {
		return 0, errors.New("ledger: account name is required")
	}
	if !kind.Valid() {
		return 0, fmt.Errorf("ledger: %q is not an account kind", kind)
	}

	var id AccountID
	err := s.db.QueryRowContext(ctx, insertAccount, name, string(kind), time.Now().UTC()).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create account %q: %w", name, err)
	}
	return id, nil
}

// EnsureAccount returns the id of an account, creating it if it does not exist.
//
// Needed because the chart of accounts grows on demand: a new tenant means a
// new receivable account, and a new meter means a new revenue account, neither
// of which can be known in advance.
//
// If the account already exists with a DIFFERENT kind, that is an error rather
// than a silent adoption. An account changing from revenue to asset would flip
// the sign every report reads it with, and the existing postings would not
// change to match.
func (s *Store) EnsureAccount(ctx context.Context, name string, kind AccountKind) (AccountID, error) {
	return ensureAccount(ctx, s.db, name, kind)
}

// EnsureAccountTx is EnsureAccount inside a caller's transaction, so a billing
// run that creates accounts and posts to them is all-or-nothing.
func (s *Store) EnsureAccountTx(ctx context.Context, tx *sql.Tx, name string, kind AccountKind) (AccountID, error) {
	return ensureAccount(ctx, tx, name, kind)
}

// querier is the part of *sql.DB and *sql.Tx this package needs, so the same
// code serves both without duplicating it.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func ensureAccount(ctx context.Context, q querier, name string, kind AccountKind) (AccountID, error) {
	if name == "" {
		return 0, errors.New("ledger: account name is required")
	}
	if !kind.Valid() {
		return 0, fmt.Errorf("ledger: %q is not an account kind", kind)
	}

	var id AccountID
	var existingKind string
	err := q.QueryRowContext(ctx, ensureAccountSQL, name, string(kind), time.Now().UTC()).
		Scan(&id, &existingKind)
	if err != nil {
		return 0, fmt.Errorf("ensure account %q: %w", name, err)
	}

	if AccountKind(existingKind) != kind {
		return 0, fmt.Errorf("ledger: account %q already exists as %q, refusing to treat it as %q",
			name, existingKind, kind)
	}
	return id, nil
}

// AccountByName looks up an account.
func (s *Store) AccountByName(ctx context.Context, name string) (Account, error) {
	var a Account
	var kind string

	err := s.db.QueryRowContext(ctx, selectAccountByName, name).Scan(&a.ID, &a.Name, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, fmt.Errorf("%w: %q", ErrAccountNotFound, name)
	}
	if err != nil {
		return Account{}, fmt.Errorf("look up account %q: %w", name, err)
	}
	a.Kind = AccountKind(kind)
	return a, nil
}

// Post records a transaction, returning its id and whether it was already
// present.
//
// Idempotent on the transaction's key: posting the same transaction twice
// stores it once and reports the second attempt as already posted. That is
// invariant I2 at the ledger boundary.
func (s *Store) Post(ctx context.Context, t *Transaction) (id int64, alreadyPosted bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin: %w", err)
	}
	// A no-op after a successful commit, so it is safe unconditionally and
	// removes any chance of an early return leaking an open transaction.
	defer tx.Rollback()

	id, alreadyPosted, err = s.PostTx(ctx, tx, t)
	if err != nil {
		return 0, false, err
	}

	// CRITICAL: the deferred balance trigger fires here, not at the inserts
	// inside PostTx. If the postings do not sum to zero, this is where it is
	// caught -- so a failure from Commit is not necessarily an infrastructure
	// problem, it may be the database refusing to let the books go out of
	// balance.
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit transaction: %w", err)
	}
	return id, alreadyPosted, nil
}

// PostTx records a transaction inside a caller's transaction.
//
// Exists so that posting to the ledger can be atomic with whatever else the
// caller is doing -- issuing an invoice and marking the events it billed, for
// instance. Splitting those across two transactions would leave a window where
// an invoice exists with no ledger entry behind it, or the reverse.
//
// The caller owns the commit, and therefore owns the moment the deferred
// balance trigger fires.
func (s *Store) PostTx(ctx context.Context, tx *sql.Tx, t *Transaction) (id int64, alreadyPosted bool, err error) {
	// Re-checked here rather than trusted from construction. This is the last
	// point before the numbers become permanent, and a Transaction could in
	// principle have reached this call from a future code path that did not go
	// through NewTransaction.
	total, err := t.Balance()
	if err != nil {
		return 0, false, err
	}
	if total != 0 {
		return 0, false, fmt.Errorf("%w: postings sum to %s", ErrUnbalanced, total)
	}

	err = tx.QueryRowContext(ctx, insertTransaction,
		t.IdempotencyKey, t.OccurredAt, t.Description, time.Now().UTC()).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		// DO NOTHING returns no row on conflict, which means this key is
		// already posted. Fetch the existing id and stop -- re-inserting the
		// postings would double every amount in it.
		if err := tx.QueryRowContext(ctx, selectTransactionByKey, t.IdempotencyKey).Scan(&id); err != nil {
			return 0, false, fmt.Errorf("look up existing transaction: %w", err)
		}
		return id, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert transaction: %w", err)
	}

	for i, p := range t.postings {
		if _, err := tx.ExecContext(ctx, insertPosting, id, int64(p.Account), int64(p.Amount)); err != nil {
			return 0, false, fmt.Errorf("insert posting %d: %w", i, err)
		}
	}
	return id, false, nil
}

// Balance returns an account's balance as a signed amount, where positive is a
// net debit.
//
// Computed by summing the account's postings rather than read from a stored
// total. There is no running balance to drift out of step with the journal, and
// the journal is the record of what actually happened.
func (s *Store) Balance(ctx context.Context, account AccountID) (Amount, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx, selectAccountBalance, int64(account)).Scan(&total); err != nil {
		return 0, fmt.Errorf("balance for account %d: %w", account, err)
	}
	return Amount(total), nil
}

// NormalBalance returns an account's balance signed the way an accountant
// would read it, so that a revenue account with credits shows a positive
// figure.
func (s *Store) NormalBalance(ctx context.Context, account Account) (Amount, error) {
	signed, err := s.Balance(ctx, account.ID)
	if err != nil {
		return 0, err
	}
	if account.Kind.IsDebitNormal() {
		return signed, nil
	}
	return signed.Neg()
}

// TrialBalance sums every posting in the ledger.
//
// CRITICAL: this must always be exactly zero. It is invariant I1 stated as a
// single query -- if it is ever non-zero, money has been created or destroyed
// somewhere and every figure derived from this ledger is suspect.
func (s *Store) TrialBalance(ctx context.Context) (Amount, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx, selectTrialBalance).Scan(&total); err != nil {
		return 0, fmt.Errorf("trial balance: %w", err)
	}
	return Amount(total), nil
}
