// Package ledger implements double-entry accounting.
//
// Double-entry is not bookkeeping ceremony. It is a redundancy check: every
// amount is recorded twice, in opposite directions, so a single arithmetic or
// logic error makes the books visibly fail to balance instead of quietly
// producing a wrong number. Invariant I1 -- money is conserved -- is that
// check, and this package exists to make it hold.
//
// See ADR-0010 for the model and why the API is shaped the way it is.
package ledger

import (
	"errors"
	"fmt"
	"time"
)

// AccountID identifies an account.
type AccountID int64

// AccountKind classifies an account, which decides which direction counts as
// an increase.
type AccountKind string

const (
	Asset     AccountKind = "asset"
	Liability AccountKind = "liability"
	Revenue   AccountKind = "revenue"
	Expense   AccountKind = "expense"
	Equity    AccountKind = "equity"
)

// IsDebitNormal reports whether a debit increases this kind of account.
//
// Assets and expenses grow with debits; liabilities, revenue, and equity grow
// with credits. This is the one piece of accounting convention the package
// encodes, and it exists only so that a balance can be presented with the sign
// a human expects -- the arithmetic underneath never consults it.
func (k AccountKind) IsDebitNormal() bool {
	return k == Asset || k == Expense
}

// Valid reports whether the kind is one of the five.
func (k AccountKind) Valid() bool {
	switch k {
	case Asset, Liability, Revenue, Expense, Equity:
		return true
	default:
		return false
	}
}

// Account is a named bucket that postings accumulate in.
type Account struct {
	ID   AccountID
	Name string
	Kind AccountKind
}

// Transfer moves an amount from one account to another.
//
// CRITICAL: this type is the reason an unbalanced transaction cannot be
// expressed.
//
// A transfer names both sides and one amount, so it necessarily produces one
// debit and one credit of equal size. A transaction is built only from
// transfers, so it balances by construction rather than by a validation step
// that some future code path might skip. There is no exported way to attach a
// lone posting to a transaction.
//
// Multi-legged entries still work: debiting one account and crediting two is
// two transfers sharing a debit account.
type Transfer struct {
	// Debit is the account charged. For an asset or expense account this is
	// an increase.
	Debit AccountID

	// Credit is the account credited. For revenue, a liability, or equity this
	// is an increase.
	Credit AccountID

	// Amount must be strictly positive. A negative transfer would be a
	// transfer in the other direction written confusingly, and allowing both
	// spellings makes every posting harder to read.
	Amount Amount
}

// Posting is one side of a transfer, as stored.
//
// The amount is signed: positive is a debit, negative is a credit. That choice
// makes "this transaction balances" the same statement as "these postings sum
// to zero", which is a far easier property to assert -- in Go, in SQL, and in
// a test -- than comparing two separately accumulated totals.
type Posting struct {
	Account AccountID
	Amount  Amount
}

// Transaction is a balanced set of postings recorded together.
//
// The postings field is unexported and only ever written by NewTransaction, so
// a caller cannot construct one directly or mutate one afterwards.
type Transaction struct {
	// IdempotencyKey makes posting the same transaction twice a no-op. This is
	// invariant I2 at the ledger boundary -- defence in depth behind the dedup
	// already done at ingest, because the ledger is where being wrong costs
	// money.
	IdempotencyKey string

	// OccurredAt is event time: when the thing being recorded happened. It
	// decides which period the transaction belongs to, exactly as
	// occurred_at does for a usage event.
	OccurredAt time.Time

	Description string

	postings []Posting
}

var (
	// ErrUnbalanced means a transaction's postings do not sum to zero. It
	// should be unreachable through the public API and exists because a
	// guarantee worth having is worth checking.
	ErrUnbalanced = errors.New("ledger: transaction does not balance")

	// ErrInvalidTransfer means a transfer could not be used.
	ErrInvalidTransfer = errors.New("ledger: invalid transfer")

	// ErrNoTransfers means a transaction was built with nothing in it.
	ErrNoTransfers = errors.New("ledger: transaction has no transfers")
)

// NewTransaction builds a balanced transaction from one or more transfers.
func NewTransaction(idempotencyKey string, occurredAt time.Time, description string, transfers ...Transfer) (*Transaction, error) {
	if idempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotency key is required", ErrInvalidTransfer)
	}
	if len(transfers) == 0 {
		return nil, ErrNoTransfers
	}
	if occurredAt.IsZero() {
		return nil, fmt.Errorf("%w: occurred_at is required", ErrInvalidTransfer)
	}

	postings := make([]Posting, 0, len(transfers)*2)

	for i, t := range transfers {
		if t.Amount <= 0 {
			return nil, fmt.Errorf("%w: transfer %d has amount %s, must be positive", ErrInvalidTransfer, i, t.Amount)
		}
		if t.Debit == t.Credit {
			return nil, fmt.Errorf("%w: transfer %d debits and credits the same account %d", ErrInvalidTransfer, i, t.Debit)
		}
		if t.Debit == 0 || t.Credit == 0 {
			return nil, fmt.Errorf("%w: transfer %d has an unset account", ErrInvalidTransfer, i)
		}

		credit, err := t.Amount.Neg()
		if err != nil {
			return nil, fmt.Errorf("transfer %d: %w", i, err)
		}

		postings = append(postings,
			Posting{Account: t.Debit, Amount: t.Amount},
			Posting{Account: t.Credit, Amount: credit},
		)
	}

	tx := &Transaction{
		IdempotencyKey: idempotencyKey,
		OccurredAt:     occurredAt,
		Description:    description,
		postings:       postings,
	}

	// Belt and braces. Construction from transfers makes this impossible to
	// fail, and it is checked anyway: the cost is one pass over a short slice,
	// and the thing being protected is whether money is conserved.
	//
	// Note what this does NOT catch: overflow is already structurally
	// impossible here. Postings are appended as +a, -a, +b, -b, so the running
	// total alternates between one transfer's amount and zero and never exceeds
	// the largest single transfer. The checked arithmetic in Add still earns
	// its place for callers accumulating in some other order -- per account,
	// for instance -- where a total genuinely can run past int64.
	total, err := tx.Balance()
	if err != nil {
		return nil, err
	}
	if total != 0 {
		return nil, fmt.Errorf("%w: postings sum to %s", ErrUnbalanced, total)
	}

	return tx, nil
}

// Postings returns a copy of the transaction's postings.
//
// A copy, not the slice itself: handing out the internal slice would let a
// caller rewrite an amount after construction and quietly unbalance a
// transaction the type system promised was balanced.
func (t *Transaction) Postings() []Posting {
	out := make([]Posting, len(t.postings))
	copy(out, t.postings)
	return out
}

// Balance sums the transaction's postings. A balanced transaction returns zero.
func (t *Transaction) Balance() (Amount, error) {
	var total Amount
	for _, p := range t.postings {
		next, err := total.Add(p.Amount)
		if err != nil {
			return 0, fmt.Errorf("summing postings: %w", err)
		}
		total = next
	}
	return total, nil
}
