package ledger

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"testing"

	"github.com/RasheedHD/LedgerLine/internal/testdb"
)

type fixture struct {
	store *Store
	db    *sql.DB

	receivable AccountID
	revenue    AccountID
	cash       AccountID
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	db := testdb.New(t)
	store := NewStore(db)
	ctx := context.Background()

	receivable, err := store.CreateAccount(ctx, "accounts_receivable", Asset)
	if err != nil {
		t.Fatalf("create receivable: %v", err)
	}
	revenue, err := store.CreateAccount(ctx, "usage_revenue", Revenue)
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	cash, err := store.CreateAccount(ctx, "cash", Asset)
	if err != nil {
		t.Fatalf("create cash: %v", err)
	}

	return fixture{store: store, db: db, receivable: receivable, revenue: revenue, cash: cash}
}

// The ordinary path: recognising revenue debits receivable and credits revenue,
// and the balances reflect it.
func TestPostAndBalance(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	tx, err := NewTransaction("usage-1", testNow, "api calls for August",
		Transfer{Debit: f.receivable, Credit: f.revenue, Amount: mustAmount(t, "12.50")})
	if err != nil {
		t.Fatalf("NewTransaction: %v", err)
	}

	id, already, err := f.store.Post(ctx, tx)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if already {
		t.Error("a first post reported as already posted")
	}
	if id == 0 {
		t.Error("no transaction id returned")
	}

	got, err := f.store.Balance(ctx, f.receivable)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := mustAmount(t, "12.50"); got != want {
		t.Errorf("receivable balance = %s, want %s", got, want)
	}

	// Revenue is credit-normal, so its signed balance is negative and its
	// normal balance is positive.
	signed, err := f.store.Balance(ctx, f.revenue)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := mustAmount(t, "-12.50"); signed != want {
		t.Errorf("revenue signed balance = %s, want %s", signed, want)
	}

	account, err := f.store.AccountByName(ctx, "usage_revenue")
	if err != nil {
		t.Fatalf("AccountByName: %v", err)
	}
	normal, err := f.store.NormalBalance(ctx, account)
	if err != nil {
		t.Fatalf("NormalBalance: %v", err)
	}
	if want := mustAmount(t, "12.50"); normal != want {
		t.Errorf("revenue normal balance = %s, want %s -- an accountant reads credit-normal accounts positive", normal, want)
	}
}

// INVARIANT I1 as a single query. Whatever has been posted, every posting in
// the ledger must sum to exactly zero.
func TestTrialBalanceIsAlwaysZero(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	rng := rand.New(rand.NewSource(7))
	accounts := []AccountID{f.receivable, f.revenue, f.cash}

	for i := 0; i < 200; i++ {
		debit := accounts[rng.Intn(len(accounts))]
		credit := debit
		for credit == debit {
			credit = accounts[rng.Intn(len(accounts))]
		}

		tx, err := NewTransaction(fmt.Sprintf("tx-%03d", i), testNow, "generated",
			Transfer{Debit: debit, Credit: credit, Amount: Amount(1 + rng.Int63n(50_000_000))})
		if err != nil {
			t.Fatalf("NewTransaction %d: %v", i, err)
		}
		if _, _, err := f.store.Post(ctx, tx); err != nil {
			t.Fatalf("Post %d: %v", i, err)
		}

		total, err := f.store.TrialBalance(ctx)
		if err != nil {
			t.Fatalf("TrialBalance: %v", err)
		}
		if total != 0 {
			t.Fatalf("after %d transactions the trial balance is %s, want 0 -- money was created or destroyed", i+1, total)
		}
	}
}

// Posting the same transaction twice must store it once. This is invariant I2
// at the ledger boundary: ingest already deduplicates, and this is the backstop
// behind it, because the ledger is where being wrong costs money.
func TestPostIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	build := func() *Transaction {
		tx, err := NewTransaction("usage-1", testNow, "api calls",
			Transfer{Debit: f.receivable, Credit: f.revenue, Amount: mustAmount(t, "12.50")})
		if err != nil {
			t.Fatalf("NewTransaction: %v", err)
		}
		return tx
	}

	firstID, already, err := f.store.Post(ctx, build())
	if err != nil {
		t.Fatalf("first post: %v", err)
	}
	if already {
		t.Error("first post reported as already posted")
	}

	secondID, already, err := f.store.Post(ctx, build())
	if err != nil {
		t.Fatalf("second post: %v", err)
	}
	if !already {
		t.Error("second post was not reported as already posted")
	}
	if secondID != firstID {
		t.Errorf("ids differ: %d then %d", firstID, secondID)
	}

	got, err := f.store.Balance(ctx, f.receivable)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := mustAmount(t, "12.50"); got != want {
		t.Fatalf("receivable balance = %s, want %s -- the amount was posted twice", got, want)
	}

	var postings int
	if err := f.db.QueryRow("SELECT count(*) FROM ledger_postings").Scan(&postings); err != nil {
		t.Fatalf("count postings: %v", err)
	}
	if postings != 2 {
		t.Errorf("posting count = %d, want 2", postings)
	}
}

// The database must refuse an unbalanced entry even when the Go API is
// bypassed entirely.
//
// The Go type makes this impossible to express, which is the primary defence.
// This proves the second one, and it is the one that matters for a repair
// script, a migration, or any future service writing to these tables directly.
func TestDatabaseRejectsUnbalancedPostingsWrittenDirectly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	var txID int64
	err = tx.QueryRow(`
		INSERT INTO ledger_transactions (idempotency_key, occurred_at, description, created_at)
		VALUES ('hand-written', $1, 'bypassing the API', $1) RETURNING id`, testNow).Scan(&txID)
	if err != nil {
		t.Fatalf("insert transaction: %v", err)
	}

	// One side only. The trigger is deferred, so this insert succeeds and the
	// failure arrives at COMMIT -- which is the whole reason it has to be
	// deferred: postings land one at a time and are unbalanced in between.
	if _, err := tx.Exec(`
		INSERT INTO ledger_postings (transaction_id, account_id, amount)
		VALUES ($1, $2, 5000000)`, txID, int64(f.receivable)); err != nil {
		t.Fatalf("insert posting: %v -- the deferred trigger fired too early", err)
	}

	if err := tx.Commit(); err == nil {
		t.Fatal("the database accepted a one-sided entry; the balance trigger is not protecting anything")
	}

	// Nothing may have survived the rejected commit.
	if total, err := f.store.TrialBalance(ctx); err != nil || total != 0 {
		t.Errorf("trial balance = %s (err %v), want 0", total, err)
	}
}

// A balanced entry written directly must still be accepted, or the trigger is
// simply rejecting everything and the test above proves nothing.
func TestDatabaseAcceptsBalancedPostingsWrittenDirectly(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	var txID int64
	err = tx.QueryRow(`
		INSERT INTO ledger_transactions (idempotency_key, occurred_at, description, created_at)
		VALUES ('hand-written-ok', $1, 'balanced by hand', $1) RETURNING id`, testNow).Scan(&txID)
	if err != nil {
		t.Fatalf("insert transaction: %v", err)
	}

	for _, p := range []struct {
		account AccountID
		amount  int64
	}{
		{f.receivable, 5_000_000},
		{f.revenue, -5_000_000},
	} {
		if _, err := tx.Exec(`
			INSERT INTO ledger_postings (transaction_id, account_id, amount)
			VALUES ($1, $2, $3)`, txID, int64(p.account), p.amount); err != nil {
			t.Fatalf("insert posting: %v", err)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("a balanced hand-written entry was rejected: %v", err)
	}
	if total, err := f.store.TrialBalance(ctx); err != nil || total != 0 {
		t.Errorf("trial balance = %s (err %v), want 0", total, err)
	}
}

// A zero-amount posting records nothing while balancing trivially, so the
// column refuses it outright.
func TestDatabaseRejectsZeroPosting(t *testing.T) {
	f := newFixture(t)

	tx, err := f.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()

	var txID int64
	err = tx.QueryRow(`
		INSERT INTO ledger_transactions (idempotency_key, occurred_at, description, created_at)
		VALUES ('zero', $1, 'zero posting', $1) RETURNING id`, testNow).Scan(&txID)
	if err != nil {
		t.Fatalf("insert transaction: %v", err)
	}

	if _, err := tx.Exec(`
		INSERT INTO ledger_postings (transaction_id, account_id, amount)
		VALUES ($1, $2, 0)`, txID, int64(f.receivable)); err == nil {
		t.Error("a zero-amount posting was accepted")
	}
}

// A balance read from the journal must equal the same figure accumulated
// independently. If these ever disagree, one of them is not describing the
// journal.
func TestBalanceMatchesIndependentReplay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	rng := rand.New(rand.NewSource(11))
	var expected Amount

	for i := 0; i < 100; i++ {
		amount := Amount(1 + rng.Int63n(9_000_000))

		tx, err := NewTransaction(fmt.Sprintf("replay-%03d", i), testNow, "generated",
			Transfer{Debit: f.receivable, Credit: f.revenue, Amount: amount})
		if err != nil {
			t.Fatalf("NewTransaction %d: %v", i, err)
		}
		if _, _, err := f.store.Post(ctx, tx); err != nil {
			t.Fatalf("Post %d: %v", i, err)
		}

		next, err := expected.Add(amount)
		if err != nil {
			t.Fatalf("accumulate: %v", err)
		}
		expected = next
	}

	got, err := f.store.Balance(ctx, f.receivable)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if got != expected {
		t.Errorf("stored balance %s != independently accumulated %s", got, expected)
	}
}

func TestAccountNotFound(t *testing.T) {
	f := newFixture(t)

	if _, err := f.store.AccountByName(context.Background(), "no_such_account"); err == nil {
		t.Fatal("looking up a missing account succeeded")
	}
}
