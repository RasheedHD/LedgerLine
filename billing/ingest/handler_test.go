package ingest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/event"
	brokerlog "github.com/RasheedHD/LedgerLine/broker/log"
)

// newTestHandler wires the handler to a real log and a frozen clock.
//
// A real log on disk rather than a fake: the thing being tested is that an
// event survives encoding, framing, and an fsync, and a fake would only prove
// the fake behaves as written.
//
// SyncGroup because that is what the handler documents it requires -- a 202
// returned on the strength of an append is only honest if the append is
// durable.
func newTestHandler(t *testing.T) (*Handler, *brokerlog.Log) {
	t.Helper()

	l, err := brokerlog.OpenDurable(t.TempDir(), brokerlog.Options{Sync: brokerlog.SyncGroup})
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	// The handler keeps the durable type; the test reads through the plain
	// one, which is all a reader needs.
	return &Handler{log: l, now: func() time.Time { return testNow }}, l.Log
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

// readEvent pulls one record back off the log and decodes it.
func readEvent(t *testing.T, l *brokerlog.Log, offset uint64) *event.UsageEvent {
	t.Helper()
	record, err := l.Read(offset)
	if err != nil {
		t.Fatalf("read offset %d: %v", offset, err)
	}
	e, err := event.Decode(record)
	if err != nil {
		t.Fatalf("decode offset %d: %v", offset, err)
	}
	return e
}

const validBody = `{"tenant_id":"acme","meter":"api_calls","quantity":"1",` +
	`"occurred_at":"2026-08-03T11:00:00Z","idempotency_key":"key-1"}`

// A well-formed event lands on the log intact, and the response carries its
// offset.
func TestAcceptsAndAppends(t *testing.T) {
	h, l := newTestHandler(t)

	rec := post(t, h, validBody)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body: %s)", rec.Code, rec.Body)
	}
	got := decodeAccepted(t, rec)
	if got.Offset != 0 {
		t.Errorf("offset = %d, want 0 for the first record", got.Offset)
	}
	if l.NextOffset() != 1 {
		t.Fatalf("log holds %d records, want 1", l.NextOffset())
	}

	e := readEvent(t, l, got.Offset)
	if e.TenantID != "acme" || e.Meter != "api_calls" || e.Quantity != "1" {
		t.Errorf("event fields did not survive the round trip: %+v", e)
	}
	if e.IdempotencyKey != "key-1" {
		t.Errorf("idempotency_key = %q, want key-1", e.IdempotencyKey)
	}
	if len(e.Fingerprint) != 32 {
		t.Errorf("fingerprint length = %d, want 32 (SHA-256)", len(e.Fingerprint))
	}
}

// received_at is stamped by the server at the moment of durable custody, which
// is now the log append rather than a database insert.
func TestServerStampsReceivedAt(t *testing.T) {
	h, l := newTestHandler(t)

	post(t, h, validBody)
	e := readEvent(t, l, 0)

	if !e.ReceivedAt.Equal(testNow) {
		t.Errorf("received_at = %s, want the server clock %s", e.ReceivedAt, testNow)
	}
	if !e.OccurredAt.Before(e.ReceivedAt) {
		t.Errorf("occurred_at %s should precede received_at %s for this fixture", e.OccurredAt, e.ReceivedAt)
	}
}

// THE BEHAVIOURAL CHANGE from ADR-0012, stated as a test.
//
// A retry is appended again rather than recognised. Ingest cannot see the
// events table, so it has no way to know this key has been seen -- and both
// records are correct input to a consumer that deduplicates. What the client
// loses is being told; what it keeps is that it is still not billed twice.
func TestRetryIsAppendedAgainAndNotReportedAsDuplicate(t *testing.T) {
	h, l := newTestHandler(t)

	first := post(t, h, validBody)
	second := post(t, h, validBody)

	if first.Code != http.StatusAccepted || second.Code != http.StatusAccepted {
		t.Fatalf("statuses = %d, %d; want 202 twice", first.Code, second.Code)
	}

	a, b := decodeAccepted(t, first), decodeAccepted(t, second)
	if a.Offset == b.Offset {
		t.Errorf("both requests reported offset %d; each append gets its own", a.Offset)
	}
	if l.NextOffset() != 2 {
		t.Fatalf("log holds %d records, want 2 -- ingest appends without deduplicating", l.NextOffset())
	}

	// Both records carry the same key and the same fingerprint, which is
	// exactly what lets the consumer collapse them.
	firstEvent, secondEvent := readEvent(t, l, a.Offset), readEvent(t, l, b.Offset)
	if firstEvent.IdempotencyKey != secondEvent.IdempotencyKey {
		t.Error("the retry carried a different idempotency key")
	}
	if string(firstEvent.Fingerprint) != string(secondEvent.Fingerprint) {
		t.Error("the retry produced a different fingerprint; the consumer would treat it as key reuse")
	}
}

// Concurrent requests each get their own offset and their own record. A
// duplicate or lost offset here would mean an event silently overwritten.
func TestConcurrentRequestsGetDistinctOffsets(t *testing.T) {
	h, l := newTestHandler(t)

	const requests = 50
	offsets := make([]uint64, requests)

	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := post(t, h, validBody)
			if rec.Code != http.StatusAccepted {
				t.Errorf("request %d: status = %d, want 202", i, rec.Code)
				return
			}
			var got acceptedResponse
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			offsets[i] = got.Offset
		}(i)
	}
	wg.Wait()

	seen := make(map[uint64]bool, requests)
	for i, offset := range offsets {
		if seen[offset] {
			t.Fatalf("request %d reused offset %d -- an event was overwritten", i, offset)
		}
		seen[offset] = true
	}
	if l.NextOffset() != requests {
		t.Errorf("log holds %d records, want %d", l.NextOffset(), requests)
	}
}

// Each rejection reaches the client as a distinct, machine-readable code, and
// none of them reaches the log. A rejected event that was appended anyway would
// be billed later by the consumer, which never saw the rejection.
func TestRejectionsAreDistinctAndNeverAppended(t *testing.T) {
	tests := []struct {
		name       string
		proves     string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "empty body",
			proves:     "an empty request is refused at decode rather than becoming an empty event",
			body:       ``,
			wantStatus: http.StatusBadRequest,
			wantCode:   codeMalformed,
		},
		{
			name:       "not json",
			proves:     "a malformed body is refused rather than partially applied",
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
			proves:     "an event that cannot be deduplicated downstream is refused here",
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
			h, l := newTestHandler(t)

			rec := post(t, h, tc.body)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)\nthis case proves: %s",
					rec.Code, tc.wantStatus, rec.Body, tc.proves)
			}
			if got := decodeError(t, rec).Error.Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q\nthis case proves: %s", got, tc.wantCode, tc.proves)
			}
			if l.NextOffset() != 0 {
				t.Errorf("log holds %d records, want 0 -- a rejected event was appended anyway", l.NextOffset())
			}
		})
	}
}

// Precision survives the whole path: JSON string, Go string, log record, and
// back. Invariant I6 -- if this fails, some layer started treating quantity as
// a number.
func TestQuantityKeepsFullPrecision(t *testing.T) {
	h, l := newTestHandler(t)

	const tiny = "0.000000001"
	post(t, h, strings.Replace(validBody, `"quantity":"1"`, `"quantity":"`+tiny+`"`, 1))

	if got := readEvent(t, l, 0).Quantity; got != tiny {
		t.Errorf("quantity = %q, want %q", got, tiny)
	}
}
