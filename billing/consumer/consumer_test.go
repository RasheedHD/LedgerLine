package consumer

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/event"
	brokerlog "github.com/RasheedHD/LedgerLine/broker/log"
	"github.com/RasheedHD/LedgerLine/internal/testdb"
)

var testNow = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func newFixture(t *testing.T, opts Options) (*Consumer, *brokerlog.Log, *sql.DB) {
	t.Helper()

	db := testdb.New(t)

	l, err := brokerlog.Open(t.TempDir(), brokerlog.Options{})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	return New("billing", l, db, opts), l, db
}

// appendEvent puts one event on the log, as ingest would.
func appendEvent(t *testing.T, l *brokerlog.Log, key, quantity string) *event.UsageEvent {
	t.Helper()

	e := &event.UsageEvent{
		TenantID:       "acme",
		Meter:          "api_calls",
		Quantity:       quantity,
		OccurredAt:     testNow.Add(-time.Hour),
		ReceivedAt:     testNow,
		IdempotencyKey: key,
	}
	e.Fingerprint = event.Fingerprint(e)

	record, err := event.Encode(e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := l.Append(record); err != nil {
		t.Fatalf("append: %v", err)
	}
	return e
}

func countEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

// The basic path: records on the log become rows in the database, and the
// offset advances past them.
func TestDrainAppliesRecords(t *testing.T) {
	c, l, db := newFixture(t, Options{})
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		appendEvent(t, l, fmt.Sprintf("key-%02d", i), "1")
	}

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if stats.Read != 25 || stats.Inserted != 25 {
		t.Errorf("stats = %+v, want 25 read and 25 inserted", stats)
	}
	if n := countEvents(t, db); n != 25 {
		t.Errorf("row count = %d, want 25", n)
	}
	if next, _ := c.NextOffset(ctx); next != 25 {
		t.Errorf("NextOffset = %d, want 25", next)
	}
}

// Draining again with nothing new must do nothing at all. A consumer that
// reprocessed on every call would work -- dedup would absorb it -- while
// quietly doing unbounded redundant work.
func TestDrainIsIdempotentWhenCaughtUp(t *testing.T) {
	c, l, db := newFixture(t, Options{})
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		appendEvent(t, l, fmt.Sprintf("key-%02d", i), "1")
	}
	if _, err := c.Drain(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("second drain: %v", err)
	}
	if stats.Read != 0 {
		t.Errorf("second drain read %d records, want 0", stats.Read)
	}
	if n := countEvents(t, db); n != 10 {
		t.Errorf("row count = %d, want 10", n)
	}
}

// CRITICAL TEST: replaying the whole log must not bill anyone twice.
//
// This is invariant I2 proved end to end, and it is what makes at-least-once
// delivery survivable. Resetting the offset to zero is exactly what a lost
// offset store, a restored backup, or a deliberate rebuild looks like.
func TestReplayFromZeroDoesNotDoubleBill(t *testing.T) {
	c, l, db := newFixture(t, Options{})
	ctx := context.Background()

	for i := 0; i < 30; i++ {
		appendEvent(t, l, fmt.Sprintf("key-%02d", i), "1")
	}
	if _, err := c.Drain(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	before := countEvents(t, db)

	// Rewind as though the offset had been lost.
	if _, err := db.Exec("UPDATE consumer_offsets SET next_offset = 0 WHERE consumer = 'billing'"); err != nil {
		t.Fatalf("rewind offset: %v", err)
	}

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("replay drain: %v", err)
	}

	if stats.Read != 30 {
		t.Errorf("replay read %d records, want 30", stats.Read)
	}
	if stats.Duplicates != 30 {
		t.Errorf("replay inserted %d new rows and saw %d duplicates; every record should have been a duplicate",
			stats.Inserted, stats.Duplicates)
	}
	if after := countEvents(t, db); after != before {
		t.Fatalf("row count went from %d to %d on replay -- the customer was billed twice", before, after)
	}
}

// The offset and the inserts must move together. If a batch fails part-way,
// neither the rows nor the offset may survive it.
func TestFailedBatchCommitsNothing(t *testing.T) {
	c, l, db := newFixture(t, Options{BatchSize: 50})
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		appendEvent(t, l, fmt.Sprintf("key-%02d", i), "1")
	}

	// Append a record the consumer will choke on: valid framing, but the
	// quantity is not a number Postgres will accept.
	bad := &event.UsageEvent{
		TenantID:       "acme",
		Meter:          "api_calls",
		Quantity:       "not-a-number",
		OccurredAt:     testNow.Add(-time.Hour),
		ReceivedAt:     testNow,
		IdempotencyKey: "key-bad",
	}
	record, err := event.Encode(bad)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := l.Append(record); err != nil {
		t.Fatalf("append: %v", err)
	}

	if _, err := c.Drain(ctx); err == nil {
		t.Fatal("drain succeeded despite an unusable record")
	}

	// The ten good records shared a transaction with the bad one, so none of
	// them may have landed, and the offset must not have moved.
	if n := countEvents(t, db); n != 0 {
		t.Errorf("row count = %d, want 0 -- a failed batch left rows behind", n)
	}
	next, err := c.NextOffset(ctx)
	if err != nil {
		t.Fatalf("next offset: %v", err)
	}
	if next != 0 {
		t.Errorf("NextOffset = %d, want 0 -- a failed batch advanced the offset", next)
	}
}

// An interrupted drain must resume exactly where it stopped: nothing lost,
// nothing applied twice. Batch boundaries are where a crash is most likely to
// be observable, so the batch size is set small to force several.
func TestInterruptedDrainResumesCleanly(t *testing.T) {
	c, l, db := newFixture(t, Options{BatchSize: 5})

	for i := 0; i < 40; i++ {
		appendEvent(t, l, fmt.Sprintf("key-%02d", i), "1")
	}

	// A context cancelled part-way stands in for the process dying mid-drain.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()
	_, _ = c.Drain(ctx)

	// Whatever it managed, the offset and the row count must agree.
	partial, err := c.NextOffset(context.Background())
	if err != nil {
		t.Fatalf("next offset: %v", err)
	}
	if n := countEvents(t, db); uint64(n) != partial {
		t.Fatalf("offset is %d but %d rows exist -- the two are not moving together", partial, n)
	}

	stats, err := c.Drain(context.Background())
	if err != nil {
		t.Fatalf("resumed drain: %v", err)
	}
	if stats.Duplicates != 0 {
		t.Errorf("resumed drain saw %d duplicates; it reprocessed records it had already committed", stats.Duplicates)
	}
	if n := countEvents(t, db); n != 40 {
		t.Errorf("row count = %d, want 40", n)
	}
	if next, _ := c.NextOffset(context.Background()); next != 40 {
		t.Errorf("NextOffset = %d, want 40", next)
	}
}

// A key reused for different content is dropped rather than stored, and
// counted so it is visible. The event that was already stored must not be
// altered.
func TestReusedKeyWithDifferentContentIsReported(t *testing.T) {
	c, l, db := newFixture(t, Options{})
	ctx := context.Background()

	appendEvent(t, l, "key-1", "1")
	appendEvent(t, l, "key-1", "999")

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if stats.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", stats.Conflicts)
	}
	if n := countEvents(t, db); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}

	var quantity string
	if err := db.QueryRow("SELECT quantity::text FROM events").Scan(&quantity); err != nil {
		t.Fatalf("read quantity: %v", err)
	}
	if quantity != "1.000000000" {
		t.Errorf("stored quantity = %q, want the original 1.000000000", quantity)
	}
}

// Two consumers track their positions independently, so adding a reader does
// not disturb billing.
func TestConsumersAreIndependent(t *testing.T) {
	billing, l, db := newFixture(t, Options{})
	ctx := context.Background()

	for i := 0; i < 10; i++ {
		appendEvent(t, l, fmt.Sprintf("key-%02d", i), "1")
	}
	if _, err := billing.Drain(ctx); err != nil {
		t.Fatalf("billing drain: %v", err)
	}

	other := New("analytics", l, db, Options{})
	if next, err := other.NextOffset(ctx); err != nil || next != 0 {
		t.Errorf("second consumer starts at %d (err %v), want 0", next, err)
	}
	if next, _ := billing.NextOffset(ctx); next != 10 {
		t.Errorf("billing offset moved to %d, want 10", next)
	}
}
