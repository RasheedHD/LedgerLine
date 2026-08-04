package ingest

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RasheedHD/LedgerLine/internal/testdb"
)

// newTestHandler wires the handler to a real database and a frozen clock.
//
// Real Postgres rather than a mock, because every claim being tested here --
// that a duplicate is caught, that a quantity keeps its precision, that
// concurrent writers cannot both win -- lives in the database rather than in
// Go. A mock would only prove that the mock behaves as written.
func newTestHandler(t *testing.T) (*Handler, *sql.DB) {
	t.Helper()
	db := testdb.New(t)
	return &Handler{db: db, now: func() time.Time { return testNow }}, db
}

func post(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/events", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeAccepted(t *testing.T, rec *httptest.ResponseRecorder) acceptedResponse {
	t.Helper()
	var got acceptedResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode accepted response %q: %v", rec.Body.String(), err)
	}
	return got
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) errorResponse {
	t.Helper()
	var got errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error response %q: %v", rec.Body.String(), err)
	}
	return got
}

func countEvents(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM events").Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

const validBody = `{"tenant_id":"acme","meter":"api_calls","quantity":"1",` +
	`"occurred_at":"2026-08-03T11:00:00Z","idempotency_key":"key-1"}`

// A first, well-formed event is stored and acknowledged as new.
func TestAcceptsNewEvent(t *testing.T) {
	h, db := newTestHandler(t)

	rec := post(t, h, validBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body)
	}
	got := decodeAccepted(t, rec)
	if got.Duplicate {
		t.Error("duplicate = true on a first request, want false")
	}
	if got.ID == 0 {
		t.Error("id = 0, want the identity value assigned by Postgres")
	}
	if n := countEvents(t, db); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}
}

// The whole point of the endpoint: a retry gets the same answer as the
// original, and does not bill the customer twice. This is invariant I2.
func TestRetryIsIdempotent(t *testing.T) {
	h, db := newTestHandler(t)

	first := post(t, h, validBody)
	second := post(t, h, validBody)

	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("statuses = %d, %d; want 202 twice -- a client that did nothing wrong must not be told the server broke",
			first.Code, second.Code)
	}

	a, b := decodeAccepted(t, first), decodeAccepted(t, second)
	if a.Duplicate {
		t.Error("first request reported duplicate = true")
	}
	if !b.Duplicate {
		t.Error("second request reported duplicate = false; the client cannot tell a retry landed")
	}
	if a.ID != b.ID {
		t.Errorf("ids differ: %d then %d; a retry must resolve to the same stored event", a.ID, b.ID)
	}
	if n := countEvents(t, db); n != 1 {
		t.Errorf("row count = %d, want 1 -- the customer was billed twice", n)
	}
}

// The test that earns ADR-0002's rejection of check-then-insert. Concurrent
// retries are the realistic case -- a client with a connection timeout and a
// retry loop generates exactly this -- and a SELECT-then-INSERT implementation
// passes TestRetryIsIdempotent while failing here.
func TestConcurrentRetriesProduceOneRow(t *testing.T) {
	h, db := newTestHandler(t)

	const attempts = 50

	var wg sync.WaitGroup
	codes := make([]int, attempts)
	ids := make([]int64, attempts)

	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := post(t, h, validBody)
			codes[i] = rec.Code
			if rec.Code == http.StatusAccepted {
				var got acceptedResponse
				_ = json.Unmarshal(rec.Body.Bytes(), &got)
				ids[i] = got.ID
			}
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != http.StatusAccepted {
			t.Errorf("attempt %d: status = %d, want 202", i, code)
		}
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Errorf("attempt %d: id = %d, want %d -- concurrent retries resolved to different events", i, id, ids[0])
		}
	}
	if n := countEvents(t, db); n != 1 {
		t.Fatalf("row count = %d, want 1 -- %d concurrent retries were billed as separate usage", n, n)
	}
}

// Identical usage under different keys is not a duplicate. ADR-0001 section 2:
// two API calls in the same millisecond are genuinely 2 units, and this is the
// case that content-hashing would have collapsed into 1.
func TestDifferentKeysAreDistinctEvents(t *testing.T) {
	h, db := newTestHandler(t)

	post(t, h, validBody)
	post(t, h, strings.Replace(validBody, `"key-1"`, `"key-2"`, 1))

	if n := countEvents(t, db); n != 2 {
		t.Errorf("row count = %d, want 2 -- identical usage under a fresh key is real usage, not a retry", n)
	}
}

// Keys are scoped per tenant, so two tenants generating the same key string do
// not collide -- and one tenant cannot probe another's key space by observing
// which of its requests come back as duplicates.
func TestKeysAreScopedPerTenant(t *testing.T) {
	h, db := newTestHandler(t)

	post(t, h, validBody)
	rec := post(t, h, strings.Replace(validBody, `"acme"`, `"globex"`, 1))

	if got := decodeAccepted(t, rec); got.Duplicate {
		t.Error("a different tenant reusing the same key string was reported as a duplicate")
	}
	if n := countEvents(t, db); n != 2 {
		t.Errorf("row count = %d, want 2", n)
	}
}

// The hole ADR-0002 identified and ADR-0005 closes. Reusing a key for
// genuinely different usage must be refused, not waved through -- accepting it
// discards billable usage while reporting success, which undercharges the
// customer silently and so is never reported.
func TestKeyReusedWithDifferentPayloadIsRejected(t *testing.T) {
	h, db := newTestHandler(t)

	if rec := post(t, h, validBody); rec.Code != http.StatusAccepted {
		t.Fatalf("first request: status = %d, want 202", rec.Code)
	}

	// Same key, different quantity.
	rec := post(t, h, strings.Replace(validBody, `"quantity":"1"`, `"quantity":"999"`, 1))

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 -- divergent usage was accepted as a duplicate (body: %s)",
			rec.Code, rec.Body)
	}
	if got := decodeError(t, rec).Error.Code; got != codeKeyReuse {
		t.Errorf("code = %q, want %q", got, codeKeyReuse)
	}
	if n := countEvents(t, db); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}

	// The stored event must be untouched. A rejected reuse that silently
	// overwrote the original would be worse than accepting it.
	var quantity string
	if err := db.QueryRow("SELECT quantity::text FROM events").Scan(&quantity); err != nil {
		t.Fatalf("read quantity: %v", err)
	}
	if quantity != "1.000000000" {
		t.Errorf("stored quantity = %q, want the original 1.000000000", quantity)
	}
}

// Reuse detection must not fire on formatting. A client sending "1" and then
// "1.0" on its retry has sent the same event twice, and rejecting that would
// break the retry path this whole endpoint exists to support.
func TestRetryWithEquivalentFormattingIsStillADuplicate(t *testing.T) {
	h, db := newTestHandler(t)

	post(t, h, validBody)
	rec := post(t, h, strings.Replace(validBody, `"quantity":"1"`, `"quantity":"1.000"`, 1))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 -- a reformatted retry was mistaken for key reuse (body: %s)",
			rec.Code, rec.Body)
	}
	if !decodeAccepted(t, rec).Duplicate {
		t.Error("duplicate = false, want true")
	}
	if n := countEvents(t, db); n != 1 {
		t.Errorf("row count = %d, want 1", n)
	}
}

// Every new row carries a fingerprint; NULL is reserved for rows written
// before migration 000002.
func TestFingerprintIsStored(t *testing.T) {
	h, db := newTestHandler(t)

	post(t, h, validBody)

	var stored []byte
	if err := db.QueryRow("SELECT payload_fingerprint FROM events").Scan(&stored); err != nil {
		t.Fatalf("read fingerprint: %v", err)
	}
	if len(stored) != 32 {
		t.Fatalf("fingerprint length = %d, want 32 (SHA-256)", len(stored))
	}
}

// Precision survives the whole path: JSON string, Go string, NUMERIC(38,9).
// Invariant I6 -- if this ever fails, some layer started treating quantity as
// a number.
func TestQuantityKeepsFullPrecision(t *testing.T) {
	h, db := newTestHandler(t)

	const tiny = "0.000000001"
	post(t, h, strings.Replace(validBody, `"quantity":"1"`, `"quantity":"`+tiny+`"`, 1))

	var stored string
	if err := db.QueryRow("SELECT quantity::text FROM events").Scan(&stored); err != nil {
		t.Fatalf("read quantity: %v", err)
	}
	if stored != tiny {
		t.Errorf("stored quantity = %q, want %q", stored, tiny)
	}
}

// received_at is ours and is stamped by the server. A client cannot know when
// we took durable custody, and every late-event decision is arithmetic on this
// value.
func TestServerStampsReceivedAt(t *testing.T) {
	h, db := newTestHandler(t)

	post(t, h, validBody)

	var receivedAt, occurredAt time.Time
	err := db.QueryRow("SELECT received_at, occurred_at FROM events").Scan(&receivedAt, &occurredAt)
	if err != nil {
		t.Fatalf("read timestamps: %v", err)
	}
	if !receivedAt.Equal(testNow) {
		t.Errorf("received_at = %s, want the server clock %s", receivedAt, testNow)
	}
	if !occurredAt.Before(receivedAt) {
		t.Errorf("occurred_at %s should precede received_at %s for this fixture", occurredAt, receivedAt)
	}
}

// Each rejection reaches the client as a distinct, machine-readable code, and
// none of them reaches Postgres. ADR-0001 requires that accepted, too-old, and
// malformed be tellable apart.
func TestRejectionsAreDistinctAndNeverStored(t *testing.T) {
	tests := []struct {
		name       string
		proves     string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "empty body",
			proves:     "an empty request no longer fails as a numeric syntax error inside Postgres",
			body:       ``,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeMalformed,
		},
		{
			name:       "not json",
			proves:     "a malformed body is refused at decode rather than partially applied",
			body:       `{oops`,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeMalformed,
		},
		{
			name:       "misspelled field",
			proves:     `"quanity" would otherwise be dropped silently and the event billed as zero`,
			body:       strings.Replace(validBody, `"quantity"`, `"quanity"`, 1),
			wantStatus: http.StatusBadRequest,
			wantCode:   codeMalformed,
		},
		{
			name:       "client supplies received_at",
			proves:     "ingest time is ours; a client cannot assert when we took custody",
			body:       strings.Replace(validBody, `{`, `{"received_at":"2026-08-03T11:00:00Z",`, 1),
			wantStatus: http.StatusBadRequest,
			wantCode:   codeMalformed,
		},
		{
			name:       "missing idempotency_key",
			proves:     "an event that cannot be deduplicated is refused rather than stored",
			body:       `{"tenant_id":"acme","meter":"api_calls","quantity":"1","occurred_at":"2026-08-03T11:00:00Z"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalid,
		},
		{
			name:       "negative quantity",
			proves:     "usage cannot be negative; a credit is a ledger concern",
			body:       strings.Replace(validBody, `"quantity":"1"`, `"quantity":"-1"`, 1),
			wantStatus: http.StatusBadRequest,
			wantCode:   codeInvalid,
		},
		{
			name:       "occurred_at beyond the backfill window",
			proves:     "too-old is a separate outcome from malformed, so a client knows to escalate rather than retry",
			body:       strings.Replace(validBody, `"2026-08-03T11:00:00Z"`, `"2026-01-01T00:00:00Z"`, 1),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   codeTooOld,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, db := newTestHandler(t)

			rec := post(t, h, tc.body)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)\nthis case proves: %s",
					rec.Code, tc.wantStatus, rec.Body, tc.proves)
			}
			if got := decodeError(t, rec).Error.Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q\nthis case proves: %s", got, tc.wantCode, tc.proves)
			}
			if n := countEvents(t, db); n != 0 {
				t.Errorf("row count = %d, want 0 -- a rejected event was stored anyway", n)
			}
		})
	}
}
