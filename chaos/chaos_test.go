package chaos

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"testing"
	"time"
)

// assertInvariants fails the test with every violation found, not just the
// first. Under chaos, several usually break together and the first one is
// rarely the most informative.
func assertInvariants(t *testing.T, h *Harness) {
	t.Helper()

	for _, v := range CheckAll(context.Background(), h) {
		t.Errorf("%s", v)
	}
}

// postN sends n events with distinct keys.
func postN(t *testing.T, h *Harness, prefix string, n int) {
	t.Helper()

	for i := 0; i < n; i++ {
		if _, err := h.Post(fmt.Sprintf("%s-%04d", prefix, i)); err != nil {
			t.Fatalf("post %d: %v", i, err)
		}
	}
}

// assertInvoiceToTheCent is the README's claim, checked.
//
// The expected total is arithmetic over what ingest acknowledged -- one unit
// per accepted key at one cent -- computed without consulting anything the
// system stored.
func assertInvoiceToTheCent(t *testing.T, h *Harness) {
	t.Helper()

	h.DrainFully(t)
	period := h.EnsurePeriod(t)
	invoice := h.CloseWithRetries(t, period.ID)

	want := h.ExpectedTotal(t)
	if invoice.Total != want {
		t.Fatalf("invoice total is %s, want %s\n%s", invoice.Total, want, h.Diagnose(t))
	}

	// And the ledger agrees with the invoice.
	receivable, err := h.Ledger.AccountByName(context.Background(), "receivable:"+Tenant)
	if err != nil {
		t.Fatalf("AccountByName: %v", err)
	}
	balance, err := h.Ledger.Balance(context.Background(), receivable.ID)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if balance != want {
		t.Errorf("receivable is %s, want %s", balance, want)
	}
}

// FAULT: the consumer is interrupted at random moments mid-drain.
//
// The realistic version of a deploy, an OOM kill, or a node being drained. The
// offset and the events it covers must move together, so an interrupted drain
// resumes without losing or repeating anything.
func TestConsumerInterruptedRepeatedly(t *testing.T) {
	h := NewHarness(t)
	rng := rand.New(rand.NewSource(1))

	postN(t, h, "interrupt", 120)

	for round := 0; round < 15; round++ {
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(1+rng.Intn(12))*time.Millisecond)
		_, _ = h.Consumer.Drain(ctx)
		cancel()

		assertInvariants(t, h)
	}

	assertInvoiceToTheCent(t, h)
	assertInvariants(t, h)
}

// FAULT: every database connection is terminated, repeatedly, while the
// consumer works.
//
// This is a failover or a connection reaper. Transactions die part-way, which
// is the window where a half-applied batch would show up.
func TestDatabaseConnectionsKilledDuringDrain(t *testing.T) {
	h := NewHarness(t)

	postN(t, h, "kill", 80)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 6; i++ {
			time.Sleep(15 * time.Millisecond)
			h.KillDatabaseConnections(t)
		}
	}()

	// Drain against a database that keeps having its legs taken away.
	for i := 0; i < 30; i++ {
		_, _ = h.Consumer.Drain(context.Background())
		time.Sleep(5 * time.Millisecond)
	}
	wg.Wait()

	assertInvoiceToTheCent(t, h)
	assertInvariants(t, h)
}

// FAULT: the broker delivers every record twice.
//
// At-least-once delivery makes this certain rather than hypothetical. The
// duplicates are byte-identical replays of records already on the log, which is
// exactly what a redelivering broker produces.
func TestEveryRecordDeliveredTwice(t *testing.T) {
	h := NewHarness(t)

	postN(t, h, "twice", 60)

	original := h.Log.NextOffset()
	for offset := uint64(0); offset < original; offset++ {
		h.AppendDuplicate(t, offset)
	}
	if h.Log.NextOffset() != original*2 {
		t.Fatalf("log holds %d records, want %d", h.Log.NextOffset(), original*2)
	}

	assertInvoiceToTheCent(t, h)
	assertInvariants(t, h)

	// The customer is billed for 60 events, not 120, despite 120 deliveries.
	var rows int
	if err := h.DB.QueryRow(`SELECT count(*) FROM events`).Scan(&rows); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if rows != 60 {
		t.Errorf("event count is %d, want 60 -- redelivery was billed", rows)
	}
}

// FAULT: the consumer offset is rewound repeatedly, as a restored backup or a
// lost offset store would do.
//
// Every rewind replays records already applied. Nothing may be billed twice.
func TestOffsetRewoundRepeatedly(t *testing.T) {
	h := NewHarness(t)
	rng := rand.New(rand.NewSource(7))

	postN(t, h, "rewind", 90)
	h.DrainFully(t)

	for round := 0; round < 10; round++ {
		h.RewindConsumer(t, uint64(rng.Intn(90)))
		h.DrainFully(t)
		assertInvariants(t, h)
	}

	assertInvoiceToTheCent(t, h)
	assertInvariants(t, h)
}

// FAULT: the period close is interrupted mid-transaction, repeatedly.
//
// Closing writes the ledger entry, the invoice, its line items, the event
// marks, and the state change. Interrupting it is how you find out whether
// those are genuinely one transaction: a half-closed period would show up as an
// invoice with no ledger entry, events marked as billed with nothing billing
// them, or a period claiming closed with no invoice at all.
func TestCloseInterruptedRepeatedly(t *testing.T) {
	h := NewHarness(t)
	rng := rand.New(rand.NewSource(3))

	postN(t, h, "close", 100)
	h.DrainFully(t)

	period := h.EnsurePeriod(t)

	// Cut the close off at an arbitrary point, over and over. Each attempt
	// either commits everything or nothing.
	for round := 0; round < 20; round++ {
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(1+rng.Intn(8))*time.Millisecond)
		_, _ = h.Invoicing.Close(ctx, period.ID, h.Plan)
		cancel()

		assertInvariants(t, h)

		// No period may be closed without an invoice, and no invoice may exist
		// without a ledger transaction behind it.
		var orphanPeriods int
		err := h.DB.QueryRow(`
			SELECT count(*) FROM billing_periods p
			WHERE p.state = 'closed'
			  AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.period_id = p.id)`).Scan(&orphanPeriods)
		if err == nil && orphanPeriods != 0 {
			t.Fatalf("round %d: %d periods are closed with no invoice", round, orphanPeriods)
		}

		var orphanInvoices int
		err = h.DB.QueryRow(`
			SELECT count(*) FROM invoices i
			WHERE NOT EXISTS (
				SELECT 1 FROM ledger_transactions lt WHERE lt.id = i.ledger_transaction_id)`).Scan(&orphanInvoices)
		if err == nil && orphanInvoices != 0 {
			t.Fatalf("round %d: %d invoices have no ledger transaction", round, orphanInvoices)
		}
	}

	invoice := h.CloseWithRetries(t, period.ID)
	if want := h.ExpectedTotal(t); invoice.Total != want {
		t.Fatalf("invoice total is %s, want %s", invoice.Total, want)
	}
	assertInvariants(t, h)
}

// INVARIANT I4 under fault: an invoice that has been issued never changes,
// however much chaos follows it.
func TestClosedInvoicesSurviveEverything(t *testing.T) {
	h := NewHarness(t)

	postN(t, h, "immutable", 50)
	h.DrainFully(t)

	period := h.EnsurePeriod(t)
	invoice := h.CloseWithRetries(t, period.ID)

	before, err := SnapshotInvoices(context.Background(), h)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Now do everything unpleasant we know how to do.
	postN(t, h, "immutable-late", 30)
	h.RewindConsumer(t, 0)
	h.DrainFully(t)
	h.KillDatabaseConnections(t)
	h.DrainFully(t)
	for round := 0; round < 5; round++ {
		_, _ = h.Invoicing.Close(context.Background(), period.ID, h.Plan)
	}

	if v := CheckInvoicesUnchanged(context.Background(), h, before); v != nil {
		t.Fatalf("%s", v)
	}

	reread, err := h.Invoicing.InvoiceForPeriod(context.Background(), period.ID)
	if err != nil {
		t.Fatalf("re-read invoice: %v", err)
	}
	if reread.Total != invoice.Total {
		t.Fatalf("invoice total changed from %s to %s", invoice.Total, reread.Total)
	}
	assertInvariants(t, h)
}

// THE GRAND FINALE: everything at once.
//
// Concurrent clients retrying, the consumer running, connections being killed,
// the offset being rewound, and duplicate delivery -- all overlapping. Then the
// books are closed and the invoice must come to the cent.
//
// This is the test the README's last clause is about.
func TestEverythingAtOnce(t *testing.T) {
	h := NewHarness(t)
	rng := rand.New(rand.NewSource(11))

	const clients = 12
	const perClient = 15

	var wg sync.WaitGroup

	// Clients posting, some of them retrying the same key.
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			for i := 0; i < perClient; i++ {
				key := fmt.Sprintf("chaos-%02d-%03d", c, i)
				status, err := h.Post(key)
				if err == nil && status != http.StatusAccepted {
					t.Errorf("client %d: status %d", c, status)
				}
				// Every third event is retried, as a client with a timeout
				// would.
				if i%3 == 0 {
					_, _ = h.Post(key)
				}
			}
		}(c)
	}

	// The consumer, running throughout.
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_, _ = h.Consumer.Drain(context.Background())
				time.Sleep(3 * time.Millisecond)
			}
		}
	}()

	// Faults, injected while all of the above is in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := 0; round < 8; round++ {
			time.Sleep(time.Duration(5+rng.Intn(15)) * time.Millisecond)
			switch round % 3 {
			case 0:
				h.KillDatabaseConnections(t)
			case 1:
				if next := h.Log.NextOffset(); next > 4 {
					h.RewindConsumer(t, uint64(rng.Intn(int(next/2))))
				}
			case 2:
				if next := h.Log.NextOffset(); next > 0 {
					h.AppendDuplicate(t, uint64(rng.Intn(int(next))))
				}
			}
		}
	}()

	// Wait for the posting clients and the fault injector, then stop the
	// consumer loop.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	assertInvoiceToTheCent(t, h)
	assertInvariants(t, h)

	t.Logf("survived: %d keys acknowledged, %d log records, invoice %s",
		h.AcceptedCount(), h.Log.NextOffset(), h.ExpectedTotal(t))
}
