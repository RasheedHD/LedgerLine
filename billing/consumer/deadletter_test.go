package consumer

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/RasheedHD/LedgerLine/billing/event"
	brokerlog "github.com/RasheedHD/LedgerLine/broker/log"
)

func countDeadLetters(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM dead_letters").Scan(&n); err != nil {
		t.Fatalf("count dead letters: %v", err)
	}
	return n
}

// appendRaw puts arbitrary bytes on the log, for records that are not valid
// events.
func appendRaw(t *testing.T, l *brokerlog.Log, payload []byte) {
	t.Helper()
	if _, err := l.Append(payload); err != nil {
		t.Fatalf("append: %v", err)
	}
}

// A reused key is recorded with enough context to act on: the reason, the
// tenant, the key, and the raw bytes of the event that was dropped.
func TestKeyReuseIsDeadLettered(t *testing.T) {
	c, l, db := newFixture(t, Options{})
	ctx := context.Background()

	appendEvent(t, l, "key-1", "1")
	appendEvent(t, l, "key-1", "999")

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if stats.Conflicts != 1 || stats.DeadLettered != 1 {
		t.Fatalf("stats = %+v, want 1 conflict and 1 dead letter", stats)
	}

	letters, err := c.UnresolvedDeadLetters(ctx, 10)
	if err != nil {
		t.Fatalf("UnresolvedDeadLetters: %v", err)
	}
	if len(letters) != 1 {
		t.Fatalf("got %d dead letters, want 1", len(letters))
	}

	got := letters[0]
	if got.Reason != ReasonKeyReuse {
		t.Errorf("reason = %q, want %q", got.Reason, ReasonKeyReuse)
	}
	if got.Offset != 1 {
		t.Errorf("offset = %d, want 1", got.Offset)
	}
	if got.TenantID != "acme" || got.IdempotencyKey != "key-1" {
		t.Errorf("tenant/key = %q/%q, want acme/key-1 -- a dead letter must be searchable by the customer who complains",
			got.TenantID, got.IdempotencyKey)
	}

	// The raw record must be recoverable, or the dead letter says only "offset
	// 1 failed" and nobody can act on it.
	dropped, err := event.Decode(got.Record)
	if err != nil {
		t.Fatalf("stored record does not decode: %v", err)
	}
	if dropped.Quantity != "999" {
		t.Errorf("stored record quantity = %q, want 999 -- the DROPPED event must be the one preserved", dropped.Quantity)
	}

	// The event that did land is untouched.
	var quantity string
	if err := db.QueryRow("SELECT quantity::text FROM events").Scan(&quantity); err != nil {
		t.Fatalf("read quantity: %v", err)
	}
	if quantity != "1.000000000" {
		t.Errorf("stored quantity = %q, want the original 1.000000000", quantity)
	}
}

// A record that cannot be decoded is dead-lettered rather than wedging the
// consumer, and its bytes are kept so the cause can be diagnosed.
func TestUndecodableRecordIsDeadLettered(t *testing.T) {
	c, l, db := newFixture(t, Options{})
	ctx := context.Background()

	appendEvent(t, l, "key-1", "1")
	appendRaw(t, l, []byte(`{"tenant_id":"acme","surprise_field":true}`))
	appendEvent(t, l, "key-2", "1")

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	// Crucially the consumer got PAST it -- key-2 landed.
	if stats.Inserted != 2 {
		t.Errorf("inserted = %d, want 2; the consumer stalled on the bad record", stats.Inserted)
	}
	if stats.Conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", stats.Conflicts)
	}
	if n := countEvents(t, db); n != 2 {
		t.Errorf("row count = %d, want 2", n)
	}

	letters, err := c.UnresolvedDeadLetters(ctx, 10)
	if err != nil {
		t.Fatalf("UnresolvedDeadLetters: %v", err)
	}
	if len(letters) != 1 {
		t.Fatalf("got %d dead letters, want 1", len(letters))
	}
	if letters[0].Reason != ReasonUndecodable {
		t.Errorf("reason = %q, want %q", letters[0].Reason, ReasonUndecodable)
	}
	if letters[0].TenantID != "" {
		t.Errorf("tenant = %q, want empty -- an undecodable record has no readable tenant", letters[0].TenantID)
	}
	if string(letters[0].Record) != `{"tenant_id":"acme","surprise_field":true}` {
		t.Errorf("raw record was not preserved verbatim: %s", letters[0].Record)
	}
}

// INVARIANT I3, as an arithmetic identity.
//
// Every record read is either stored, recognised as a duplicate, or
// dead-lettered. If this ever fails, a record came off the log and vanished
// with no outcome recorded anywhere -- the silent loss I3 forbids.
func TestEveryRecordIsAccountedFor(t *testing.T) {
	c, l, db := newFixture(t, Options{BatchSize: 7})
	ctx := context.Background()

	// A deliberately awkward mixture: new events, exact retries, a reused key,
	// and two records that cannot be decoded.
	for i := 0; i < 10; i++ {
		appendEvent(t, l, fmt.Sprintf("key-%02d", i), "1")
	}
	for i := 0; i < 4; i++ {
		appendEvent(t, l, fmt.Sprintf("key-%02d", i), "1") // exact retries
	}
	appendEvent(t, l, "key-00", "42") // reused key, different content
	appendRaw(t, l, []byte("not an event at all"))
	appendRaw(t, l, []byte(`{"quantity":`))

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}

	if !stats.Accounted() {
		t.Fatalf("records unaccounted for: %+v (read %d, inserted+duplicates+conflicts %d)",
			stats, stats.Read, stats.Inserted+stats.Duplicates+stats.Conflicts)
	}
	if stats.Read != 17 {
		t.Errorf("read = %d, want 17", stats.Read)
	}
	if stats.Inserted != 10 {
		t.Errorf("inserted = %d, want 10", stats.Inserted)
	}
	if stats.Duplicates != 4 {
		t.Errorf("duplicates = %d, want 4", stats.Duplicates)
	}
	if stats.Conflicts != 3 {
		t.Errorf("conflicts = %d, want 3 (one reuse, two undecodable)", stats.Conflicts)
	}

	// And the same identity holds against the database, not just the counters.
	events, letters := countEvents(t, db), countDeadLetters(t, db)
	if events+letters != 13 {
		t.Errorf("%d events + %d dead letters = %d, want 13 distinct outcomes", events, letters, events+letters)
	}
}

// INVARIANT I5. Replaying the log must not accumulate duplicate dead letters,
// or a rebuild would grow the table every time it ran.
func TestReplayDoesNotDuplicateDeadLetters(t *testing.T) {
	c, l, db := newFixture(t, Options{})
	ctx := context.Background()

	appendEvent(t, l, "key-1", "1")
	appendEvent(t, l, "key-1", "999")
	appendRaw(t, l, []byte("garbage"))

	if _, err := c.Drain(ctx); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	before := countDeadLetters(t, db)
	if before != 2 {
		t.Fatalf("dead letters = %d, want 2", before)
	}

	if _, err := db.Exec("UPDATE consumer_offsets SET next_offset = 0 WHERE consumer = 'billing'"); err != nil {
		t.Fatalf("rewind: %v", err)
	}

	stats, err := c.Drain(ctx)
	if err != nil {
		t.Fatalf("replay drain: %v", err)
	}

	// Still counted as conflicts -- they still failed -- but no new rows.
	if stats.Conflicts != 2 {
		t.Errorf("replay conflicts = %d, want 2", stats.Conflicts)
	}
	if stats.DeadLettered != 0 {
		t.Errorf("replay wrote %d new dead letters, want 0", stats.DeadLettered)
	}
	if after := countDeadLetters(t, db); after != before {
		t.Fatalf("dead letters grew from %d to %d on replay", before, after)
	}
}

// The dead letter and the offset move together. A failed batch must leave
// neither behind, or the consumer would skip a record while claiming it had
// recorded the failure.
func TestDeadLetterAndOffsetCommitTogether(t *testing.T) {
	c, l, db := newFixture(t, Options{BatchSize: 50})
	ctx := context.Background()

	appendRaw(t, l, []byte("garbage"))

	// A record that decodes but that Postgres will refuse, failing the batch
	// after the dead letter has been written to the transaction.
	bad := &event.UsageEvent{
		TenantID:       "acme",
		Meter:          "api_calls",
		Quantity:       "not-a-number",
		OccurredAt:     testNow.Add(-1),
		ReceivedAt:     testNow,
		IdempotencyKey: "key-bad",
	}
	record, err := event.Encode(bad)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	appendRaw(t, l, record)

	if _, err := c.Drain(ctx); err == nil {
		t.Fatal("drain succeeded despite an unusable record")
	}

	if n := countDeadLetters(t, db); n != 0 {
		t.Errorf("dead letters = %d, want 0 -- a rolled-back batch left one behind", n)
	}
	next, err := c.NextOffset(ctx)
	if err != nil {
		t.Fatalf("next offset: %v", err)
	}
	if next != 0 {
		t.Errorf("offset = %d, want 0 -- a failed batch advanced past records", next)
	}
}

// Dead letters are per consumer, so a second reader failing on a record does
// not look like billing having failed on it.
func TestDeadLettersAreScopedPerConsumer(t *testing.T) {
	billing, l, db := newFixture(t, Options{})
	ctx := context.Background()

	appendRaw(t, l, []byte("garbage"))

	if _, err := billing.Drain(ctx); err != nil {
		t.Fatalf("billing drain: %v", err)
	}

	other := New("analytics", l, db, Options{})
	letters, err := other.UnresolvedDeadLetters(ctx, 10)
	if err != nil {
		t.Fatalf("UnresolvedDeadLetters: %v", err)
	}
	if len(letters) != 0 {
		t.Errorf("the second consumer sees %d dead letters, want 0", len(letters))
	}

	if _, err := other.Drain(ctx); err != nil {
		t.Fatalf("analytics drain: %v", err)
	}
	if n := countDeadLetters(t, db); n != 2 {
		t.Errorf("dead letters = %d, want 2 (one per consumer)", n)
	}
}
