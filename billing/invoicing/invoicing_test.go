package invoicing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/ledger"
	"github.com/RasheedHD/LedgerLine/billing/pricing"
	"github.com/RasheedHD/LedgerLine/internal/testdb"
)

var (
	augStart = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	augEnd   = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sepEnd   = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
)

type fixture struct {
	svc    *Service
	ledger *ledger.Store
	db     *sql.DB
}

func newFixture(t *testing.T) fixture {
	t.Helper()

	db := testdb.New(t)
	ledgerStore := ledger.NewStore(db)

	registry, err := pricing.NewRegistry(
		pricing.Meter{Name: "api_calls", Unit: "call"},
		pricing.Meter{Name: "gb_egress", Unit: "GB"},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	return fixture{svc: New(db, ledgerStore, registry), ledger: ledgerStore, db: db}
}

func mustAmount(t *testing.T, s string) ledger.Amount {
	t.Helper()
	a, err := ledger.ParseAmount(s)
	if err != nil {
		t.Fatalf("ParseAmount(%q): %v", s, err)
	}
	return a
}

func insertEvent(t *testing.T, db *sql.DB, tenant, meter, quantity string, occurredAt time.Time, key string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO events (tenant_id, meter, quantity, occurred_at, received_at, idempotency_key, payload_fingerprint)
		VALUES ($1, $2, $3::numeric, $4, $5, $6, $7)`,
		tenant, meter, quantity, occurredAt, occurredAt.Add(time.Second), key, []byte("fp"))
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func flatPlan(t *testing.T) pricing.Plan {
	t.Helper()
	return pricing.Plan{Name: "standard", Prices: []pricing.Price{
		{Meter: "api_calls", Model: pricing.Flat, UnitPrice: mustAmount(t, "0.01")},
		{Meter: "gb_egress", Model: pricing.Flat, UnitPrice: mustAmount(t, "0.10")},
	}}
}

func august(t *testing.T, f fixture) Period {
	t.Helper()
	p, err := f.svc.EnsurePeriod(context.Background(), "acme", "2026-08", augStart, augEnd)
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	return p
}

// Closing a period turns usage into an invoice, a ledger entry, and a mark on
// every event it billed.
func TestCloseIssuesAnInvoice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		insertEvent(t, f.db, "acme", "api_calls", "1", augStart.Add(time.Hour), fmt.Sprintf("k-%03d", i))
	}
	insertEvent(t, f.db, "acme", "gb_egress", "5", augStart.Add(2*time.Hour), "gb-1")

	invoice, err := f.svc.Close(ctx, august(t, f).ID, flatPlan(t))
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 100 calls at 0.01 = 1.00, plus 5 GB at 0.10 = 0.50.
	if want := mustAmount(t, "1.50"); invoice.Total != want {
		t.Errorf("total = %s, want %s", invoice.Total, want)
	}
	if len(invoice.LineItems) != 2 {
		t.Fatalf("got %d line items, want 2", len(invoice.LineItems))
	}
	if invoice.LedgerTransactionID == 0 {
		t.Error("invoice has no ledger transaction behind it")
	}

	// Every event is marked as billed by this invoice.
	var unbilled int
	if err := f.db.QueryRow("SELECT count(*) FROM events WHERE invoice_id IS NULL").Scan(&unbilled); err != nil {
		t.Fatalf("count unbilled: %v", err)
	}
	if unbilled != 0 {
		t.Errorf("%d events remain unbilled after closing", unbilled)
	}

	// The period is closed.
	period, err := f.svc.EnsurePeriod(ctx, "acme", "2026-08", augStart, augEnd)
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	if period.State != Closed {
		t.Errorf("state = %q, want %q", period.State, Closed)
	}

	// INVARIANT I1 after billing.
	if total, err := f.ledger.TrialBalance(ctx); err != nil || total != 0 {
		t.Errorf("trial balance = %s (err %v), want 0", total, err)
	}

	// The receivable equals the invoice.
	receivable, err := f.ledger.AccountByName(ctx, ReceivableAccount("acme"))
	if err != nil {
		t.Fatalf("AccountByName: %v", err)
	}
	balance, err := f.ledger.Balance(ctx, receivable.ID)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != invoice.Total {
		t.Errorf("receivable %s != invoice total %s", balance, invoice.Total)
	}
}

// Closing twice returns the original invoice and bills nothing further. This is
// what makes a retried billing job safe.
func TestCloseIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", augStart.Add(time.Hour), "k-1")
	period := august(t, f)

	first, err := f.svc.Close(ctx, period.ID, flatPlan(t))
	if err != nil {
		t.Fatalf("first close: %v", err)
	}

	second, err := f.svc.Close(ctx, period.ID, flatPlan(t))
	if !errors.Is(err, ErrPeriodClosed) {
		t.Fatalf("second close error = %v, want ErrPeriodClosed", err)
	}
	if second.ID != first.ID || second.Total != first.Total {
		t.Errorf("second close returned a different invoice: %+v vs %+v", second, first)
	}

	var invoices int
	if err := f.db.QueryRow("SELECT count(*) FROM invoices").Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if invoices != 1 {
		t.Fatalf("invoice count = %d, want 1", invoices)
	}

	receivable, _ := f.ledger.AccountByName(ctx, ReceivableAccount("acme"))
	balance, _ := f.ledger.Balance(ctx, receivable.ID)
	if want := mustAmount(t, "1.00"); balance != want {
		t.Errorf("receivable = %s, want %s -- the period was billed twice", balance, want)
	}
}

// CLOSES D39, which was a live bug: usage arriving for an already-closed period
// used to be silently never billed.
//
// It is now still unbilled, so the next period picks it up. That is also
// ADR-0001 §5's late-event roll-forward, which falls out of the model rather
// than needing a special case.
func TestLateUsageRollsIntoTheNextPeriod(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", augStart.Add(time.Hour), "on-time")

	augustInvoice, err := f.svc.Close(ctx, august(t, f).ID, flatPlan(t))
	if err != nil {
		t.Fatalf("close august: %v", err)
	}
	if want := mustAmount(t, "1.00"); augustInvoice.Total != want {
		t.Fatalf("august total = %s, want %s", augustInvoice.Total, want)
	}

	// Usage that HAPPENED in August but only arrives now, after August closed.
	insertEvent(t, f.db, "acme", "api_calls", "50", augStart.Add(2*time.Hour), "late-arrival")

	september, err := f.svc.EnsurePeriod(ctx, "acme", "2026-09", augEnd, sepEnd)
	if err != nil {
		t.Fatalf("EnsurePeriod september: %v", err)
	}
	septemberInvoice, err := f.svc.Close(ctx, september.ID, flatPlan(t))
	if err != nil {
		t.Fatalf("close september: %v", err)
	}

	// The late usage is billed -- not lost.
	if want := mustAmount(t, "0.50"); septemberInvoice.Total != want {
		t.Fatalf("september total = %s, want %s -- late usage was not rolled forward", septemberInvoice.Total, want)
	}

	// And it is identifiable as late, so the invoice can explain itself.
	if len(septemberInvoice.LineItems) != 1 {
		t.Fatalf("got %d line items, want 1", len(septemberInvoice.LineItems))
	}
	if !septemberInvoice.LineItems[0].Late {
		t.Error("the rolled-forward line is not flagged late; the invoice cannot explain why it is there")
	}

	// INVARIANT I4: August's invoice is untouched.
	reread, err := f.svc.InvoiceForPeriod(ctx, augustInvoice.PeriodID)
	if err != nil {
		t.Fatalf("re-read august: %v", err)
	}
	if reread.Total != augustInvoice.Total {
		t.Fatalf("august total changed from %s to %s when late usage arrived",
			augustInvoice.Total, reread.Total)
	}

	if total, err := f.ledger.TrialBalance(ctx); err != nil || total != 0 {
		t.Errorf("trial balance = %s (err %v), want 0", total, err)
	}
}

// INVARIANT I4, enforced by the database rather than by convention.
//
// Everything downstream of an invoice acted on its number already. The Go code
// never updates one, but a migration or a repair script would bypass that, so
// the rule lives in the schema.
func TestInvoicesAreImmutableInTheDatabase(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", augStart.Add(time.Hour), "k-1")
	invoice, err := f.svc.Close(ctx, august(t, f).ID, flatPlan(t))
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := f.db.Exec("UPDATE invoices SET total = 1 WHERE id = $1", invoice.ID); err == nil {
		t.Error("an invoice total was updated; I4 is not protected")
	}
	if _, err := f.db.Exec("DELETE FROM invoices WHERE id = $1", invoice.ID); err == nil {
		t.Error("an invoice was deleted; I4 is not protected")
	}
	if _, err := f.db.Exec("UPDATE invoice_line_items SET amount = 1 WHERE invoice_id = $1", invoice.ID); err == nil {
		t.Error("an invoice line item was updated; I4 is not protected")
	}

	reread, err := f.svc.InvoiceForPeriod(ctx, invoice.PeriodID)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if reread.Total != invoice.Total {
		t.Errorf("total changed from %s to %s", invoice.Total, reread.Total)
	}
}

// An event may be billed once. Re-stamping it onto a second invoice would bill
// the same usage twice while every individual row still looked consistent.
func TestAnEventCannotBeBilledTwice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", augStart.Add(time.Hour), "k-1")
	invoice, err := f.svc.Close(ctx, august(t, f).ID, flatPlan(t))
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	_, err = f.db.Exec("UPDATE events SET invoice_id = $1 WHERE invoice_id = $2", invoice.ID+999, invoice.ID)
	if err == nil {
		t.Error("a billed event was moved to another invoice")
	}
}

// Two concurrent close attempts must produce one invoice. Closes D40, where
// the previous design was correct only by accident of a unique constraint.
func TestConcurrentClosesProduceOneInvoice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", augStart.Add(time.Hour), "k-1")
	period := august(t, f)

	const attempts = 8
	var wg sync.WaitGroup
	results := make([]error, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = f.svc.Close(ctx, period.ID, flatPlan(t))
		}(i)
	}
	wg.Wait()

	var succeeded int
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrPeriodClosed):
			// Expected for every loser.
		default:
			t.Errorf("attempt %d failed unexpectedly: %v", i, err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d attempts issued an invoice, want exactly 1", succeeded)
	}

	var invoices int
	if err := f.db.QueryRow("SELECT count(*) FROM invoices").Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if invoices != 1 {
		t.Fatalf("invoice count = %d, want 1", invoices)
	}

	receivable, _ := f.ledger.AccountByName(ctx, ReceivableAccount("acme"))
	balance, _ := f.ledger.Balance(ctx, receivable.ID)
	if want := mustAmount(t, "1.00"); balance != want {
		t.Errorf("receivable = %s, want %s", balance, want)
	}
}

// Usage after the period end is not billed to it, however late the close runs.
func TestUsageAfterThePeriodEndIsNotBilled(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", augStart.Add(time.Hour), "in-period")
	insertEvent(t, f.db, "acme", "api_calls", "500", augEnd.Add(time.Hour), "next-period")

	invoice, err := f.svc.Close(ctx, august(t, f).ID, flatPlan(t))
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if want := mustAmount(t, "1.00"); invoice.Total != want {
		t.Errorf("total = %s, want %s -- usage after the period end was billed to it", invoice.Total, want)
	}

	var unbilled int
	if err := f.db.QueryRow("SELECT count(*) FROM events WHERE invoice_id IS NULL").Scan(&unbilled); err != nil {
		t.Fatalf("count unbilled: %v", err)
	}
	if unbilled != 1 {
		t.Errorf("%d events unbilled, want 1 waiting for the next period", unbilled)
	}
}

// A period with nothing to bill issues no invoice rather than an empty one.
func TestNothingToBill(t *testing.T) {
	f := newFixture(t)

	_, err := f.svc.Close(context.Background(), august(t, f).ID, flatPlan(t))
	if !errors.Is(err, ErrNothingToBill) {
		t.Fatalf("error = %v, want ErrNothingToBill", err)
	}

	var invoices int
	if err := f.db.QueryRow("SELECT count(*) FROM invoices").Scan(&invoices); err != nil {
		t.Fatalf("count invoices: %v", err)
	}
	if invoices != 0 {
		t.Errorf("invoice count = %d, want 0", invoices)
	}
}

// Tenants are billed independently.
func TestPeriodsAreScopedPerTenant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", augStart.Add(time.Hour), "acme-1")
	insertEvent(t, f.db, "globex", "api_calls", "500", augStart.Add(time.Hour), "globex-1")

	if _, err := f.svc.Close(ctx, august(t, f).ID, flatPlan(t)); err != nil {
		t.Fatalf("close acme: %v", err)
	}

	globexPeriod, err := f.svc.EnsurePeriod(ctx, "globex", "2026-08", augStart, augEnd)
	if err != nil {
		t.Fatalf("EnsurePeriod globex: %v", err)
	}
	globexInvoice, err := f.svc.Close(ctx, globexPeriod.ID, flatPlan(t))
	if err != nil {
		t.Fatalf("close globex: %v", err)
	}

	if want := mustAmount(t, "5.00"); globexInvoice.Total != want {
		t.Errorf("globex total = %s, want %s", globexInvoice.Total, want)
	}
	if total, err := f.ledger.TrialBalance(ctx); err != nil || total != 0 {
		t.Errorf("trial balance = %s (err %v), want 0", total, err)
	}
}

func TestEnsurePeriodIsIdempotentAndValidated(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	first := august(t, f)
	second := august(t, f)
	if first.ID != second.ID {
		t.Errorf("EnsurePeriod created two periods: %d and %d", first.ID, second.ID)
	}

	if _, err := f.svc.EnsurePeriod(ctx, "acme", "bad", augEnd, augStart); !errors.Is(err, ErrInvalidPeriod) {
		t.Errorf("backwards period error = %v, want ErrInvalidPeriod", err)
	}
	if _, err := f.svc.EnsurePeriod(ctx, "", "x", augStart, augEnd); !errors.Is(err, ErrInvalidPeriod) {
		t.Errorf("empty tenant error = %v, want ErrInvalidPeriod", err)
	}
}
