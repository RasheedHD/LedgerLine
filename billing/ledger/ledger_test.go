package ledger

import (
	"errors"
	"math/rand"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func mustAmount(t *testing.T, s string) Amount {
	t.Helper()
	a, err := ParseAmount(s)
	if err != nil {
		t.Fatalf("ParseAmount(%q): %v", s, err)
	}
	return a
}

// INVARIANT I1, stated as a property.
//
// For any random sequence of transfers between random accounts, the resulting
// transaction sums to zero. This is what "balanced by construction" means, and
// generating the cases rather than listing them is the point: a handful of
// hand-picked examples proves the examples work, while this proves the
// construction does.
func TestTransactionsAlwaysBalance(t *testing.T) {
	rng := rand.New(rand.NewSource(20260803))

	for run := 0; run < 2000; run++ {
		transfers := make([]Transfer, 1+rng.Intn(8))
		for i := range transfers {
			debit := AccountID(1 + rng.Intn(6))

			// Any account but the debit side; a self-transfer is refused, and
			// that refusal is tested separately.
			credit := debit
			for credit == debit {
				credit = AccountID(1 + rng.Intn(6))
			}

			transfers[i] = Transfer{
				Debit:  debit,
				Credit: credit,
				Amount: Amount(1 + rng.Int63n(1_000_000_000)),
			}
		}

		tx, err := NewTransaction("key", testNow, "generated", transfers...)
		if err != nil {
			t.Fatalf("run %d: NewTransaction: %v", run, err)
		}

		total, err := tx.Balance()
		if err != nil {
			t.Fatalf("run %d: Balance: %v", run, err)
		}
		if total != 0 {
			t.Fatalf("run %d: %d transfers summed to %s, want 0", run, len(transfers), total)
		}

		// Every transfer must produce exactly two postings, one of each sign.
		if got, want := len(tx.Postings()), len(transfers)*2; got != want {
			t.Fatalf("run %d: %d postings for %d transfers, want %d", run, got, len(transfers), want)
		}
	}
}

// Debits and credits must be equal in total, not merely sum to zero -- a
// transaction of all-zero postings would satisfy the sum while recording
// nothing.
func TestDebitsEqualCredits(t *testing.T) {
	tx, err := NewTransaction("key", testNow, "split charge",
		Transfer{Debit: 1, Credit: 2, Amount: mustAmount(t, "10.50")},
		Transfer{Debit: 1, Credit: 3, Amount: mustAmount(t, "2.25")},
	)
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}

	var debits, credits Amount
	for _, p := range tx.Postings() {
		if p.Amount > 0 {
			debits += p.Amount
		} else {
			credits -= p.Amount
		}
	}

	if debits != credits {
		t.Errorf("debits %s != credits %s", debits, credits)
	}
	if want := mustAmount(t, "12.75"); debits != want {
		t.Errorf("debits = %s, want %s", debits, want)
	}
}

// A multi-legged entry -- one debit, two credits -- is expressible as two
// transfers sharing a debit account. If this did not work, the transfer model
// would be too weak for real accounting and the balanced-by-construction
// property would have been bought at too high a price.
func TestMultiLeggedEntry(t *testing.T) {
	receivable, revenue, discount := AccountID(1), AccountID(2), AccountID(3)

	tx, err := NewTransaction("invoice-1", testNow, "invoice with a discount",
		Transfer{Debit: receivable, Credit: revenue, Amount: mustAmount(t, "100")},
		Transfer{Debit: discount, Credit: receivable, Amount: mustAmount(t, "10")},
	)
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}

	net := Amount(0)
	for _, p := range tx.Postings() {
		if p.Account == receivable {
			net += p.Amount
		}
	}
	if want := mustAmount(t, "90"); net != want {
		t.Errorf("net receivable = %s, want %s", net, want)
	}
}

func TestNewTransactionRejectsBadInput(t *testing.T) {
	tests := []struct {
		name      string
		proves    string
		key       string
		occurred  time.Time
		transfers []Transfer
		wantErr   error
	}{
		{
			name:     "no transfers",
			proves:   "an empty transaction records nothing and is almost certainly a bug upstream",
			key:      "k",
			occurred: testNow,
			wantErr:  ErrNoTransfers,
		},
		{
			name:      "zero amount",
			proves:    "a zero posting balances trivially while recording nothing, which hides the mistake",
			key:       "k",
			occurred:  testNow,
			transfers: []Transfer{{Debit: 1, Credit: 2, Amount: 0}},
			wantErr:   ErrInvalidTransfer,
		},
		{
			name:      "negative amount",
			proves:    "a negative transfer is a transfer the other way written confusingly; one spelling only",
			key:       "k",
			occurred:  testNow,
			transfers: []Transfer{{Debit: 1, Credit: 2, Amount: -5}},
			wantErr:   ErrInvalidTransfer,
		},
		{
			name:      "same account both sides",
			proves:    "debiting and crediting one account nets to nothing and is never what was meant",
			key:       "k",
			occurred:  testNow,
			transfers: []Transfer{{Debit: 1, Credit: 1, Amount: 100}},
			wantErr:   ErrInvalidTransfer,
		},
		{
			name:      "unset account",
			proves:    "a zero AccountID is the zero value, so this catches a struct that was never filled in",
			key:       "k",
			occurred:  testNow,
			transfers: []Transfer{{Debit: 1, Credit: 0, Amount: 100}},
			wantErr:   ErrInvalidTransfer,
		},
		{
			name:      "missing idempotency key",
			proves:    "without a key the same transaction could be posted twice and both would stand",
			key:       "",
			occurred:  testNow,
			transfers: []Transfer{{Debit: 1, Credit: 2, Amount: 100}},
			wantErr:   ErrInvalidTransfer,
		},
		{
			name:      "missing occurred_at",
			proves:    "event time decides the period, so a transaction without one cannot be placed",
			key:       "k",
			transfers: []Transfer{{Debit: 1, Credit: 2, Amount: 100}},
			wantErr:   ErrInvalidTransfer,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewTransaction(tc.key, tc.occurred, "test", tc.transfers...)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v\nthis case proves: %s", err, tc.wantErr, tc.proves)
			}
		})
	}
}

// Postings must be a copy. Handing out the internal slice would let a caller
// rewrite an amount after construction and unbalance a transaction the type
// system promised was balanced.
func TestPostingsCannotBeMutatedThroughTheAccessor(t *testing.T) {
	tx, err := NewTransaction("k", testNow, "test",
		Transfer{Debit: 1, Credit: 2, Amount: mustAmount(t, "50")})
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}

	stolen := tx.Postings()
	stolen[0].Amount = mustAmount(t, "999999")

	total, err := tx.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if total != 0 {
		t.Fatalf("mutating the returned slice unbalanced the transaction (sum %s)", total)
	}
}

// Balance stays exact even at the largest representable amounts.
//
// Worth pinning down, because the obvious worry -- that several large transfers
// could sum past int64 -- turns out not to apply. Postings are appended in
// transfer order as +a, -a, +b, -b, so the running total alternates between a
// single transfer's amount and zero. It never exceeds the largest individual
// transfer, which is itself a valid Amount by definition.
//
// So overflow is structurally impossible here rather than merely unlikely. The
// checked arithmetic in Add stays regardless: it costs nothing, and it protects
// callers who accumulate postings in some other order.
func TestBalanceIsExactAtExtremeAmounts(t *testing.T) {
	max := Amount(1<<63 - 1)

	tx, err := NewTransaction("k", testNow, "extreme",
		Transfer{Debit: 1, Credit: 2, Amount: max},
		Transfer{Debit: 1, Credit: 3, Amount: max},
	)
	if err != nil {
		t.Fatalf("NewTransaction with maximum amounts: %v", err)
	}

	total, err := tx.Balance()
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if total != 0 {
		t.Errorf("balance = %d, want 0", total)
	}
}

// Accumulating postings in an order that DOES overflow must report it rather
// than wrap. This is the case the checked arithmetic exists for -- a wrapped
// total flips a large positive balance negative while both sides of the ledger
// wrap consistently, so every downstream check passes on nonsense.
func TestAccumulatingOutOfOrderReportsOverflow(t *testing.T) {
	max := Amount(1<<63 - 1)

	// Two debits summed before their matching credits, which is what any
	// per-account accumulation does.
	if _, err := max.Add(max); !errors.Is(err, ErrOverflow) {
		t.Fatalf("error = %v, want ErrOverflow", err)
	}
}

func TestAccountKindNormalSide(t *testing.T) {
	tests := []struct {
		kind        AccountKind
		debitNormal bool
		proves      string
	}{
		{Asset, true, "assets grow when debited"},
		{Expense, true, "expenses grow when debited"},
		{Liability, false, "liabilities grow when credited"},
		{Revenue, false, "revenue grows when credited"},
		{Equity, false, "equity grows when credited"},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			if got := tc.kind.IsDebitNormal(); got != tc.debitNormal {
				t.Errorf("IsDebitNormal = %v, want %v\nthis case proves: %s", got, tc.debitNormal, tc.proves)
			}
			if !tc.kind.Valid() {
				t.Errorf("%q reported invalid", tc.kind)
			}
		})
	}

	if AccountKind("goodwill-ish").Valid() {
		t.Error("an unknown kind was accepted")
	}
}
