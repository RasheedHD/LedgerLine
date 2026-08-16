package posting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/ledger"
	"github.com/RasheedHD/LedgerLine/billing/pricing"
	"github.com/RasheedHD/LedgerLine/internal/testdb"
)

var (
	periodStart = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
)

func august() Period {
	return Period{Label: "2026-08", Start: periodStart, End: periodEnd}
}

type fixture struct {
	poster *Poster
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

	return fixture{poster: New(db, ledgerStore, registry), ledger: ledgerStore, db: db}
}

func mustAmount(t *testing.T, s string) ledger.Amount {
	t.Helper()
	a, err := ledger.ParseAmount(s)
	if err != nil {
		t.Fatalf("ParseAmount(%q): %v", s, err)
	}
	return a
}

// insertEvent puts usage straight into the events table, as the consumer would.
func insertEvent(t *testing.T, db *sql.DB, tenant, meter, quantity string, occurredAt time.Time, key string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO events (tenant_id, meter, quantity, occurred_at, received_at, idempotency_key, payload_fingerprint)
		VALUES ($1, $2, $3::numeric, $4, $5, $6, $7)`,
		tenant, meter, quantity, occurredAt, occurredAt.Add(time.Second), key, []byte("fingerprint"))
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}
}

func flatPlan(t *testing.T) pricing.Plan {
	t.Helper()
	return pricing.Plan{
		Name: "standard",
		Prices: []pricing.Price{
			{Meter: "api_calls", Model: pricing.Flat, UnitPrice: mustAmount(t, "0.01")},
			{Meter: "gb_egress", Model: pricing.Flat, UnitPrice: mustAmount(t, "0.10")},
		},
	}
}

// The whole point of the package: usage becomes a balanced ledger entry.
func TestPostRecognisesRevenue(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 100; i++ {
		insertEvent(t, f.db, "acme", "api_calls", "1", periodStart.Add(time.Hour), fmt.Sprintf("k-%03d", i))
	}
	insertEvent(t, f.db, "acme", "gb_egress", "5", periodStart.Add(2*time.Hour), "gb-1")

	result, err := f.poster.Post(ctx, "acme", august(), flatPlan(t))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if result.AlreadyPosted {
		t.Error("a first post reported as already posted")
	}

	// 100 calls at 0.01 = 1.00, plus 5 GB at 0.10 = 0.50.
	if want := mustAmount(t, "1.50"); result.Total != want {
		t.Errorf("total = %s, want %s", result.Total, want)
	}
	if len(result.LineItems) != 2 {
		t.Fatalf("got %d line items, want 2", len(result.LineItems))
	}

	// The receivable is an asset and grows with a debit, so its balance is
	// positive and equals the invoice total.
	receivable, err := f.ledger.AccountByName(ctx, ReceivableAccount("acme"))
	if err != nil {
		t.Fatalf("AccountByName: %v", err)
	}
	balance, err := f.ledger.Balance(ctx, receivable.ID)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := mustAmount(t, "1.50"); balance != want {
		t.Errorf("receivable balance = %s, want %s", balance, want)
	}

	// Revenue is credit-normal, so it reads positive as a normal balance.
	revenue, err := f.ledger.AccountByName(ctx, RevenueAccount("api_calls"))
	if err != nil {
		t.Fatalf("AccountByName: %v", err)
	}
	normal, err := f.ledger.NormalBalance(ctx, revenue)
	if err != nil {
		t.Fatalf("NormalBalance: %v", err)
	}
	if want := mustAmount(t, "1.00"); normal != want {
		t.Errorf("api_calls revenue = %s, want %s", normal, want)
	}

	// INVARIANT I1.
	if total, err := f.ledger.TrialBalance(ctx); err != nil || total != 0 {
		t.Errorf("trial balance = %s (err %v), want 0", total, err)
	}
}

// INVARIANT I2 at the ledger boundary. Running the same posting run twice
// records the revenue once -- otherwise a retried billing job doubles every
// customer's invoice while the books still balance perfectly.
func TestPostIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", periodStart.Add(time.Hour), "k-1")

	first, err := f.poster.Post(ctx, "acme", august(), flatPlan(t))
	if err != nil {
		t.Fatalf("first post: %v", err)
	}
	second, err := f.poster.Post(ctx, "acme", august(), flatPlan(t))
	if err != nil {
		t.Fatalf("second post: %v", err)
	}

	if first.AlreadyPosted {
		t.Error("first post reported as already posted")
	}
	if !second.AlreadyPosted {
		t.Error("second post was not recognised as already posted")
	}
	if second.TransactionID != first.TransactionID {
		t.Errorf("ids differ: %d then %d", first.TransactionID, second.TransactionID)
	}

	receivable, _ := f.ledger.AccountByName(ctx, ReceivableAccount("acme"))
	balance, err := f.ledger.Balance(ctx, receivable.ID)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if want := mustAmount(t, "1.00"); balance != want {
		t.Fatalf("receivable = %s, want %s -- the run posted twice", balance, want)
	}
}

// Period bounds are half-open, so consecutive periods neither overlap nor leave
// a gap. An event at exactly midnight belongs to one period, not both.
func TestPeriodBoundsAreHalfOpen(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "1", periodStart.Add(-time.Nanosecond), "before")
	insertEvent(t, f.db, "acme", "api_calls", "10", periodStart, "at-start")
	insertEvent(t, f.db, "acme", "api_calls", "100", periodEnd.Add(-time.Nanosecond), "last-instant")
	insertEvent(t, f.db, "acme", "api_calls", "1000", periodEnd, "at-end")

	usages, err := f.poster.Usage(ctx, "acme", august())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("got %d meters, want 1", len(usages))
	}

	// The start instant is included, the end instant is not: 10 + 100.
	want, err := pricing.ParseQuantity("110")
	if err != nil {
		t.Fatalf("ParseQuantity: %v", err)
	}
	if usages[0].Quantity != want {
		t.Errorf("quantity = %s, want %s -- the boundary events landed in the wrong period",
			usages[0].Quantity, want)
	}
}

// Usage is placed by occurred_at, not received_at. That is what makes a late
// event bill against the period it actually happened in.
func TestUsageIsPlacedByEventTimeNotIngestTime(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Happened inside the period, but arrived long afterwards.
	_, err := f.db.Exec(`
		INSERT INTO events (tenant_id, meter, quantity, occurred_at, received_at, idempotency_key, payload_fingerprint)
		VALUES ('acme', 'api_calls', 7, $1, $2, 'late', '\x00')`,
		periodStart.Add(time.Hour), periodEnd.Add(72*time.Hour))
	if err != nil {
		t.Fatalf("insert late event: %v", err)
	}

	usages, err := f.poster.Usage(ctx, "acme", august())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}
	if len(usages) != 1 {
		t.Fatalf("the late event was not placed in the period it occurred in (got %d meters)", len(usages))
	}
}

// One tenant's usage never reaches another's receivable.
func TestUsageIsScopedPerTenant(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "100", periodStart.Add(time.Hour), "acme-1")
	insertEvent(t, f.db, "globex", "api_calls", "500", periodStart.Add(time.Hour), "globex-1")

	if _, err := f.poster.Post(ctx, "acme", august(), flatPlan(t)); err != nil {
		t.Fatalf("post acme: %v", err)
	}
	if _, err := f.poster.Post(ctx, "globex", august(), flatPlan(t)); err != nil {
		t.Fatalf("post globex: %v", err)
	}

	for _, tc := range []struct{ tenant, want string }{
		{"acme", "1.00"},
		{"globex", "5.00"},
	} {
		account, err := f.ledger.AccountByName(ctx, ReceivableAccount(tc.tenant))
		if err != nil {
			t.Fatalf("AccountByName %s: %v", tc.tenant, err)
		}
		balance, err := f.ledger.Balance(ctx, account.ID)
		if err != nil {
			t.Fatalf("Balance %s: %v", tc.tenant, err)
		}
		if want := mustAmount(t, tc.want); balance != want {
			t.Errorf("%s receivable = %s, want %s", tc.tenant, balance, want)
		}
	}

	if total, err := f.ledger.TrialBalance(ctx); err != nil || total != 0 {
		t.Errorf("trial balance = %s (err %v), want 0", total, err)
	}
}

// Tiered pricing is applied to the period's TOTAL, which is the reason usage is
// aggregated before it is priced. Posting per event would leave every event in
// the first tier.
func TestTieredPricingAppliesToThePeriodTotal(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	upTo := func(s string) *pricing.Quantity {
		q, err := pricing.ParseQuantity(s)
		if err != nil {
			t.Fatalf("ParseQuantity: %v", err)
		}
		return &q
	}

	plan := pricing.Plan{
		Name: "tiered",
		Prices: []pricing.Price{{
			Meter: "api_calls",
			Model: pricing.Graduated,
			Tiers: []pricing.Tier{
				{UpTo: upTo("1000"), UnitPrice: mustAmount(t, "0.01")},
				{UpTo: nil, UnitPrice: mustAmount(t, "0.001")},
			},
		}},
	}

	// 1500 calls arriving as 15 separate events of 100.
	for i := 0; i < 15; i++ {
		insertEvent(t, f.db, "acme", "api_calls", "100", periodStart.Add(time.Duration(i)*time.Hour), fmt.Sprintf("k-%02d", i))
	}

	result, err := f.poster.Post(ctx, "acme", august(), plan)
	if err != nil {
		t.Fatalf("Post: %v", err)
	}

	// 1000 at 0.01 plus 500 at 0.001 = 10.50. Per event it would be 15.00.
	if want := mustAmount(t, "10.50"); result.Total != want {
		t.Errorf("total = %s, want %s -- events were priced individually instead of as a period total",
			result.Total, want)
	}
}

// A period with no usage posts nothing rather than an empty transaction.
func TestNoUsagePostsNothing(t *testing.T) {
	f := newFixture(t)

	_, err := f.poster.Post(context.Background(), "acme", august(), flatPlan(t))
	if !errors.Is(err, ErrNothingToPost) {
		t.Fatalf("error = %v, want ErrNothingToPost", err)
	}
}

// Usage that prices to zero -- a free tier, a zero-rate meter -- posts nothing.
// A zero posting balances trivially while recording nothing, which hides
// whatever produced it.
func TestZeroPricedUsagePostsNothing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_calls", "1000", periodStart.Add(time.Hour), "free-1")

	free := pricing.Plan{
		Name:   "free",
		Prices: []pricing.Price{{Meter: "api_calls", Model: pricing.Flat, UnitPrice: 0}},
	}

	_, err := f.poster.Post(ctx, "acme", august(), free)
	if !errors.Is(err, ErrNothingToPost) {
		t.Fatalf("error = %v, want ErrNothingToPost", err)
	}

	var postings int
	if err := f.db.QueryRow("SELECT count(*) FROM ledger_postings").Scan(&postings); err != nil {
		t.Fatalf("count postings: %v", err)
	}
	if postings != 0 {
		t.Errorf("posting count = %d, want 0", postings)
	}
}

// An unregistered meter stops the run rather than being billed as nothing.
// Closes the loop on D12: the registry has to be consulted on the path that
// actually charges people.
func TestUnknownMeterStopsTheRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	insertEvent(t, f.db, "acme", "api_call", "100", periodStart.Add(time.Hour), "typo-1")

	_, err := f.poster.Post(ctx, "acme", august(), flatPlan(t))
	if !errors.Is(err, pricing.ErrUnknownMeter) {
		t.Fatalf("error = %v, want ErrUnknownMeter", err)
	}

	if total, err := f.ledger.TrialBalance(ctx); err != nil || total != 0 {
		t.Errorf("trial balance = %s (err %v), want 0", total, err)
	}
}

// Account names are namespaced, so a tenant called "api_calls" cannot collide
// with the revenue account for the api_calls meter.
func TestAccountNamespacesDoNotCollide(t *testing.T) {
	if ReceivableAccount("api_calls") == RevenueAccount("api_calls") {
		t.Fatal("a tenant and a meter with the same name share an account")
	}
}

// An account that already exists with a different kind is refused. Adopting it
// would flip the sign every report reads it with, while the existing postings
// stayed as they were.
func TestEnsureAccountRefusesAKindChange(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.ledger.EnsureAccount(ctx, "receivable:acme", ledger.Asset); err != nil {
		t.Fatalf("EnsureAccount: %v", err)
	}
	if _, err := f.ledger.EnsureAccount(ctx, "receivable:acme", ledger.Revenue); err == nil {
		t.Fatal("an account was silently adopted as a different kind")
	}

	// Repeating the original call is still fine.
	if _, err := f.ledger.EnsureAccount(ctx, "receivable:acme", ledger.Asset); err != nil {
		t.Errorf("EnsureAccount is not idempotent: %v", err)
	}
}

func TestInvalidPeriodIsRefused(t *testing.T) {
	f := newFixture(t)

	backwards := Period{Label: "bad", Start: periodEnd, End: periodStart}
	if _, err := f.poster.Usage(context.Background(), "acme", backwards); !errors.Is(err, ErrInvalidPeriod) {
		t.Errorf("error = %v, want ErrInvalidPeriod", err)
	}
}
