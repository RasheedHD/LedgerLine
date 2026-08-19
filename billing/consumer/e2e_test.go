package consumer_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/consumer"
	"github.com/RasheedHD/LedgerLine/billing/ingest"
	"github.com/RasheedHD/LedgerLine/billing/invoicing"
	"github.com/RasheedHD/LedgerLine/billing/ledger"
	"github.com/RasheedHD/LedgerLine/billing/pricing"
	brokerlog "github.com/RasheedHD/LedgerLine/broker/log"
	"github.com/RasheedHD/LedgerLine/internal/testdb"
)

// End-to-end through the real seam: HTTP handler -> broker log -> consumer ->
// Postgres. No component is faked.
//
// This is the test the project's thesis is about. Ingest cannot deduplicate --
// it never sees the events table -- so every retry really is appended to the
// log. Exactly-once effect has to come from the consumer, and this is where
// that claim either holds or does not.
//
// An external test package (consumer_test) so it imports ingest the way a real
// caller would, which also proves there is no accidental dependency the other
// way.
func newPipeline(t *testing.T) (*httptest.Server, *brokerlog.Log, *consumer.Consumer, *sql.DB) {
	t.Helper()

	db := testdb.New(t)

	// SyncGroup, as the handler documents it requires: the 202 is only honest
	// if the append behind it is durable.
	l, err := brokerlog.OpenDurable(t.TempDir(), brokerlog.Options{Sync: brokerlog.SyncGroup})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	mux := http.NewServeMux()
	mux.Handle("POST /events", ingest.NewHandler(l))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server, l.Log, consumer.New("billing", l.Log, db, consumer.Options{}), db
}

func postEvent(t *testing.T, server *httptest.Server, key, quantity string) int {
	t.Helper()

	body := fmt.Sprintf(
		`{"tenant_id":"acme","meter":"api_calls","quantity":%q,`+
			`"occurred_at":"2026-08-03T11:00:00Z","idempotency_key":%q}`,
		quantity, key)

	resp, err := http.Post(server.URL+"/events", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func countRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// INVARIANT I2, end to end.
//
// The same request sent five times is appended five times -- ingest has no way
// to know better -- and produces exactly one row. That gap between "five
// records on the log" and "one row in the database" is the entire point of the
// design.
func TestRetriesReachTheDatabaseOnce(t *testing.T) {
	server, l, c, db := newPipeline(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if status := postEvent(t, server, "key-1", "1"); status != http.StatusAccepted {
			t.Fatalf("attempt %d: status = %d, want 202", i, status)
		}
	}

	if l.NextOffset() != 5 {
		t.Fatalf("log holds %d records, want 5 -- ingest is deduplicating, which it cannot do", l.NextOffset())
	}

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if stats.Read != 5 {
		t.Errorf("consumer read %d records, want 5", stats.Read)
	}
	if stats.Inserted != 1 {
		t.Errorf("inserted %d rows, want 1", stats.Inserted)
	}
	if stats.Duplicates != 4 {
		t.Errorf("duplicates = %d, want 4", stats.Duplicates)
	}
	if n := countRows(t, db); n != 1 {
		t.Fatalf("row count = %d, want 1 -- the customer was billed %d times", n, n)
	}
}

// Distinct keys are distinct usage, even with identical content. ADR-0001
// section 2: two API calls in the same millisecond are genuinely two units.
func TestDistinctKeysAllLand(t *testing.T) {
	server, _, c, db := newPipeline(t)

	for i := 0; i < 20; i++ {
		if status := postEvent(t, server, fmt.Sprintf("key-%02d", i), "1"); status != http.StatusAccepted {
			t.Fatalf("event %d: status = %d, want 202", i, status)
		}
	}

	if _, err := c.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := countRows(t, db); n != 20 {
		t.Errorf("row count = %d, want 20", n)
	}
}

// Concurrent retries -- the realistic shape of a client with a timeout and a
// retry loop -- still produce one row.
func TestConcurrentRetriesReachTheDatabaseOnce(t *testing.T) {
	server, l, c, db := newPipeline(t)

	const attempts = 40
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if status := postEvent(t, server, "key-concurrent", "1"); status != http.StatusAccepted {
				t.Errorf("status = %d, want 202", status)
			}
		}()
	}
	wg.Wait()

	if l.NextOffset() != attempts {
		t.Fatalf("log holds %d records, want %d", l.NextOffset(), attempts)
	}

	if _, err := c.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := countRows(t, db); n != 1 {
		t.Fatalf("row count = %d, want 1 -- %d concurrent retries were billed separately", n, n)
	}
}

// A rejected request never reaches the log, so it can never reach the database
// either. The consumer only ever sees events ingest accepted.
func TestRejectedRequestsNeverReachTheDatabase(t *testing.T) {
	server, l, c, db := newPipeline(t)

	bad := []string{
		`{"tenant_id":"acme","meter":"api_calls","quantity":"-1","occurred_at":"2026-08-03T11:00:00Z","idempotency_key":"k"}`,
		`{"tenant_id":"acme","meter":"api_calls","quantity":"1","occurred_at":"2026-01-01T00:00:00Z","idempotency_key":"k"}`,
		`{not json`,
	}
	for i, body := range bad {
		resp, err := http.Post(server.URL+"/events", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted {
			t.Errorf("request %d was accepted: %s", i, body)
		}
	}

	if l.NextOffset() != 0 {
		t.Fatalf("log holds %d records, want 0", l.NextOffset())
	}
	if _, err := c.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := countRows(t, db); n != 0 {
		t.Errorf("row count = %d, want 0", n)
	}
}

// THE WHOLE SYSTEM, end to end: HTTP request -> broker log -> consumer ->
// events -> pricing -> double-entry ledger.
//
// Every component is real. This is the project's thesis reduced to one
// assertion: a client that retries produces a log full of duplicates and a
// ledger that charges exactly once, with the books balanced.
func TestFullPipelineFromHTTPToLedger(t *testing.T) {
	server, l, c, db := newPipeline(t)
	ctx := context.Background()

	// 100 billable calls, sent as 100 distinct events...
	for i := 0; i < 100; i++ {
		if status := postEvent(t, server, fmt.Sprintf("call-%03d", i), "1"); status != http.StatusAccepted {
			t.Fatalf("event %d: status = %d, want 202", i, status)
		}
	}
	// ...and 40 retries of events already sent, as a flaky client would.
	for i := 0; i < 40; i++ {
		postEvent(t, server, fmt.Sprintf("call-%03d", i), "1")
	}

	if l.NextOffset() != 140 {
		t.Fatalf("log holds %d records, want 140", l.NextOffset())
	}

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !stats.Accounted() {
		t.Fatalf("records unaccounted for: %+v", stats)
	}
	if stats.Inserted != 100 || stats.Duplicates != 40 {
		t.Fatalf("stats = %+v, want 100 inserted and 40 duplicates", stats)
	}

	registry, err := pricing.NewRegistry(pricing.Meter{Name: "api_calls", Unit: "call"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	unitPrice, err := ledger.ParseAmount("0.01")
	if err != nil {
		t.Fatalf("ParseAmount: %v", err)
	}
	plan := pricing.Plan{Name: "standard", Prices: []pricing.Price{
		{Meter: "api_calls", Model: pricing.Flat, UnitPrice: unitPrice},
	}}

	ledgerStore := ledger.NewStore(db)
	billing := invoicing.New(db, ledgerStore, registry)

	period, err := billing.EnsurePeriod(ctx, "acme", "2026-08",
		time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}

	result, err := billing.Close(ctx, period.ID, plan)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 100 calls at $0.01. The 40 retries must not appear anywhere in this
	// number -- that is invariant I2 surviving the entire pipeline.
	want, err := ledger.ParseAmount("1.00")
	if err != nil {
		t.Fatalf("ParseAmount: %v", err)
	}
	if result.Total != want {
		t.Fatalf("invoice total = %s, want %s -- retries reached the ledger", result.Total, want)
	}

	receivable, err := ledgerStore.AccountByName(ctx, invoicing.ReceivableAccount("acme"))
	if err != nil {
		t.Fatalf("AccountByName: %v", err)
	}
	balance, err := ledgerStore.Balance(ctx, receivable.ID)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != want {
		t.Errorf("receivable = %s, want %s", balance, want)
	}

	// INVARIANT I1, after the whole pipeline has run.
	total, err := ledgerStore.TrialBalance(ctx)
	if err != nil {
		t.Fatalf("TrialBalance: %v", err)
	}
	if total != 0 {
		t.Fatalf("trial balance = %s, want 0 -- money was created or destroyed", total)
	}

	// And running the billing job again changes nothing.
	again, err := billing.Close(ctx, period.ID, plan)
	if !errors.Is(err, invoicing.ErrPeriodClosed) {
		t.Errorf("second close returned %v, want ErrPeriodClosed", err)
	}
	if again.ID != result.ID {
		t.Errorf("second close returned invoice %d, want the original %d", again.ID, result.ID)
	}
	if after, _ := ledgerStore.Balance(ctx, receivable.ID); after != want {
		t.Errorf("receivable = %s after re-closing, want %s", after, want)
	}
}

// INVARIANT I5. Replaying the whole log against a database that already holds
// the events changes nothing. This is what makes the log an auditable record
// rather than a one-shot pipe.
func TestFullReplayChangesNothing(t *testing.T) {
	server, _, c, db := newPipeline(t)
	ctx := context.Background()

	for i := 0; i < 30; i++ {
		postEvent(t, server, fmt.Sprintf("key-%02d", i), "2.5")
	}
	if _, err := c.Drain(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	before := countRows(t, db)

	// Rewind, as a lost offset store or a deliberate rebuild would.
	if _, err := db.Exec("UPDATE consumer_offsets SET next_offset = 0 WHERE consumer = 'billing'"); err != nil {
		t.Fatalf("rewind: %v", err)
	}

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("replay drain: %v", err)
	}
	if stats.Inserted != 0 {
		t.Errorf("replay inserted %d new rows, want 0", stats.Inserted)
	}
	if after := countRows(t, db); after != before {
		t.Fatalf("row count went from %d to %d on replay", before, after)
	}
}

// THE AVAILABILITY PROPERTY that D31 was decided for.
//
// Ingest never touches Postgres, so a database outage cannot stop usage being
// accepted and durably stored. Simulated by draining only after the events are
// already on the log -- from ingest's point of view the database being absent
// and being merely behind are the same thing.
//
// Under the alternative design, where ingest looked up the events table to
// report duplicates, every one of these requests would have failed and the
// usage would have been lost at the client.
func TestIngestAcceptsWithoutTheConsumerRunning(t *testing.T) {
	server, l, c, db := newPipeline(t)

	for i := 0; i < 10; i++ {
		if status := postEvent(t, server, fmt.Sprintf("offline-%02d", i), "1"); status != http.StatusAccepted {
			t.Fatalf("event %d: status = %d, want 202 with no consumer running", i, status)
		}
	}

	// Durably stored despite nothing having consumed them.
	if l.NextOffset() != 10 {
		t.Fatalf("log holds %d records, want 10", l.NextOffset())
	}
	if n := countRows(t, db); n != 0 {
		t.Fatalf("row count = %d, want 0 before the consumer runs", n)
	}

	// The consumer catches up afterwards, losing nothing.
	if _, err := c.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n := countRows(t, db); n != 10 {
		t.Errorf("row count = %d, want 10 after catching up", n)
	}
}
