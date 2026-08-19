package chaos

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

// Fixed prices, so the expected invoice is arithmetic a human can check:
// one unit per event at one cent each.
const (
	UnitPrice     = "0.01"
	EventQuantity = "1"
	Tenant        = "acme"
	Meter         = "api_calls"
	PeriodLabel   = "2026-08"
)

var (
	periodStart = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	periodEnd   = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// Inside the period, and inside the backfill window ingest enforces.
	occurredAt = time.Date(2026, 8, 3, 11, 0, 0, 0, time.UTC)
)

// Harness is a whole LedgerLine, assembled from the real components, with the
// levers a chaos scenario needs to break it.
//
// Nothing here is a mock. A fault injected into a fake only proves the fake
// handles it.
type Harness struct {
	DB        *sql.DB
	Log       *brokerlog.Log
	Server    *httptest.Server
	Consumer  *consumer.Consumer
	Invoicing *invoicing.Service
	Ledger    *ledger.Store
	Plan      pricing.Plan

	mu sync.Mutex
	// Keys ingest answered 202 to. The expectation is derived from what the
	// system actually acknowledged, not from what the test meant to send.
	accepted map[string]bool
}

// NewHarness assembles a pipeline against a real database and a real log.
func NewHarness(t *testing.T) *Harness {
	t.Helper()

	db := testdb.New(t)

	// SyncGroup: ingest answers 202 on the strength of the append, so the
	// append has to be durable before it returns. Chaos with a weaker policy
	// would be testing a system nobody should run.
	l, err := brokerlog.Open(t.TempDir(), brokerlog.Options{Sync: brokerlog.SyncGroup})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	registry, err := pricing.NewRegistry(pricing.Meter{Name: Meter, Unit: "call"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	unitPrice, err := ledger.ParseAmount(UnitPrice)
	if err != nil {
		t.Fatalf("ParseAmount: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("POST /events", ingest.NewHandler(l))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	ledgerStore := ledger.NewStore(db)

	return &Harness{
		DB:        db,
		Log:       l,
		Server:    server,
		Consumer:  consumer.New("billing", l, db, consumer.Options{BatchSize: 7}),
		Invoicing: invoicing.New(db, ledgerStore, registry),
		Ledger:    ledgerStore,
		Plan: pricing.Plan{Name: "chaos", Prices: []pricing.Price{
			{Meter: Meter, Model: pricing.Flat, UnitPrice: unitPrice},
		}},
		accepted: make(map[string]bool),
	}
}

// Post sends one event and records whether ingest acknowledged it.
func (h *Harness) Post(key string) (int, error) {
	body := fmt.Sprintf(
		`{"tenant_id":%q,"meter":%q,"quantity":%q,"occurred_at":%q,"idempotency_key":%q}`,
		Tenant, Meter, EventQuantity, occurredAt.Format(time.RFC3339), key)

	resp, err := http.Post(h.Server.URL+"/events", "application/json", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusAccepted {
		h.mu.Lock()
		h.accepted[key] = true
		h.mu.Unlock()
	}
	return resp.StatusCode, nil
}

// AcceptedCount is how many distinct keys ingest acknowledged.
func (h *Harness) AcceptedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.accepted)
}

// AcceptedKeys lists the distinct keys ingest acknowledged.
func (h *Harness) AcceptedKeys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	keys := make([]string, 0, len(h.accepted))
	for key := range h.accepted {
		keys = append(keys, key)
	}
	return keys
}

// ExpectedTotal is what the invoice must come to, computed independently of
// anything the system recorded.
//
// One unit per accepted event at UnitPrice. If the suite and the system agreed
// only because both derived the number the same way, the suite would prove
// nothing.
func (h *Harness) ExpectedTotal(t *testing.T) ledger.Amount {
	t.Helper()

	unit, err := ledger.ParseAmount(UnitPrice)
	if err != nil {
		t.Fatalf("ParseAmount: %v", err)
	}
	return ledger.Amount(int64(unit) * int64(h.AcceptedCount()))
}

// CaughtUp reports whether the consumer has read the whole log.
func (h *Harness) CaughtUp(ctx context.Context) (bool, error) {
	next, err := h.Consumer.NextOffset(ctx)
	if err != nil {
		return false, err
	}
	return next >= h.Log.NextOffset(), nil
}

// DrainFully consumes until caught up, retrying through transient failures.
//
// Retries are the point: a scenario kills things mid-drain, and a real consumer
// would come back and finish. Giving up on the first error would make every
// scenario fail for the wrong reason.
func (h *Harness) DrainFully(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	for attempt := 0; attempt < 60; attempt++ {
		if _, err := h.Consumer.Drain(ctx); err != nil {
			// A killed connection surfaces here. The pool reconnects on the
			// next call, so this is expected rather than fatal.
			time.Sleep(20 * time.Millisecond)
			continue
		}
		caughtUp, err := h.CaughtUp(ctx)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if caughtUp {
			return
		}
	}
	t.Fatal("consumer never caught up after 60 attempts")
}

// KillDatabaseConnections terminates every backend this process holds, short of
// the one issuing the command.
//
// Brutal and entirely realistic: a failover, an idle-connection reaper, or an
// operator running the same statement. In-flight transactions are lost
// part-way, which is exactly the window worth testing.
func (h *Harness) KillDatabaseConnections(t *testing.T) {
	t.Helper()

	_, err := h.DB.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity ` +
		`WHERE datname = current_database() AND pid <> pg_backend_pid()`)
	if err != nil {
		// The statement can be killed by its own effects. That is the fault
		// working, not a problem with the test.
		t.Logf("kill connections returned %v (expected under chaos)", err)
	}
}

// RewindConsumer moves the committed offset backwards, as a restored backup or
// a lost offset store would.
//
// CRITICAL: LEAST, so this can only ever move the offset BACKWARD.
//
// The first version wrote the offset unconditionally, and when the consumer
// happened to be behind the chosen value it moved FORWARD -- silently skipping
// every record in between. That produced a real loss of acknowledged events and
// looked exactly like an I3 violation in the system.
//
// It was the harness lying. No real fault skips a consumer forward past
// unread records: a restored backup, a wiped offset store, and a replayed
// rebuild all go backward. A chaos suite that injects impossible faults reports
// bugs that cannot happen and buries the ones that can.
func (h *Harness) RewindConsumer(t *testing.T, to uint64) {
	t.Helper()

	_, err := h.DB.Exec(
		`UPDATE consumer_offsets SET next_offset = LEAST(next_offset, $1) WHERE consumer = 'billing'`, to)
	if err != nil {
		// Expected, not fatal. This runs on a fault-injection goroutine
		// alongside KillDatabaseConnections, so its own connection is fair game
		// -- failing here would report the suite's deliberate fault as a bug.
		//
		// t.Fatal would also be wrong on two counts: Go only permits FailNow
		// from the goroutine running the test.
		t.Logf("rewind consumer: %v (expected under chaos)", err)
	}
}

// AppendDuplicate re-appends an existing log record, simulating a broker that
// delivers the same bytes twice. At-least-once delivery makes this certain,
// not hypothetical.
func (h *Harness) AppendDuplicate(t *testing.T, offset uint64) {
	t.Helper()

	// Logged rather than fatal for the same reason as RewindConsumer: this runs
	// on a fault-injection goroutine, where t.Fatal is not permitted and a
	// transient failure is part of the scenario rather than a result.
	record, err := h.Log.Read(offset)
	if err != nil {
		t.Logf("read offset %d: %v (expected under chaos)", offset, err)
		return
	}
	if _, err := h.Log.Append(record); err != nil {
		t.Logf("re-append offset %d: %v (expected under chaos)", offset, err)
	}
}

// EnsurePeriod creates the billing period every scenario bills into.
func (h *Harness) EnsurePeriod(t *testing.T) invoicing.Period {
	t.Helper()

	period, err := h.Invoicing.EnsurePeriod(context.Background(), Tenant, PeriodLabel, periodStart, periodEnd)
	if err != nil {
		t.Fatalf("EnsurePeriod: %v", err)
	}
	return period
}

// CloseWithRetries closes the period, retrying through transient failures, and
// returns the resulting invoice.
func (h *Harness) CloseWithRetries(t *testing.T, periodID int64) invoicing.Invoice {
	t.Helper()

	for attempt := 0; attempt < 60; attempt++ {
		invoice, err := h.Invoicing.Close(context.Background(), periodID, h.Plan)
		if err == nil || errors.Is(err, invoicing.ErrPeriodClosed) {
			return invoice
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("period never closed after 60 attempts")
	return invoicing.Invoice{}
}

// Diagnose reports where every acknowledged event actually ended up.
//
// A chaos failure that says only "the total is wrong" sends you back to
// guessing. This says whether the events are missing, unconsumed, or
// dead-lettered, which is usually the whole answer.
func (h *Harness) Diagnose(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	var events, billed, dead int
	_ = h.DB.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&events)
	_ = h.DB.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE invoice_id IS NOT NULL`).Scan(&billed)
	_ = h.DB.QueryRowContext(ctx, `SELECT count(*) FROM dead_letters`).Scan(&dead)

	consumerOffset, _ := h.Consumer.NextOffset(ctx)

	return fmt.Sprintf(
		"  acknowledged: %d\n  log records:  %d\n  consumer at:  %d\n"+
			"  event rows:   %d\n  billed rows:  %d\n  dead letters: %d",
		h.AcceptedCount(), h.Log.NextOffset(), consumerOffset, events, billed, dead)
}
