package ingest

import (
	"strings"
	"testing"
	"time"
)

// Every time-based case is expressed relative to this instant so the suite
// behaves identically whenever it runs.
var testNow = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func validRequest() eventRequest {
	return eventRequest{
		TenantID:       "acme",
		Meter:          "api_calls",
		Quantity:       "1",
		OccurredAt:     testNow.Add(-time.Hour),
		IdempotencyKey: "key-1",
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name     string
		proves   string
		mutate   func(*eventRequest)
		wantCode string // "" means the request should be accepted
	}{
		{
			name:   "valid request",
			proves: "a well-formed event passes every rule, so later failures are about the rule under test and not the fixture",
			mutate: func(r *eventRequest) {},
		},
		{
			name:     "missing tenant_id",
			proves:   "a required field is caught here rather than becoming a NOT NULL violation inside Postgres",
			mutate:   func(r *eventRequest) { r.TenantID = "" },
			wantCode: codeInvalid,
		},
		{
			name:     "missing meter",
			proves:   "the same required-field rule applies to every string field, not just the first one checked",
			mutate:   func(r *eventRequest) { r.Meter = "" },
			wantCode: codeInvalid,
		},
		{
			name:     "missing idempotency_key",
			proves:   "an event without a key cannot be deduplicated, so it is refused rather than stored",
			mutate:   func(r *eventRequest) { r.IdempotencyKey = "" },
			wantCode: codeInvalid,
		},
		{
			name:     "tenant_id with trailing whitespace",
			proves:   `"acme" and "acme " would otherwise be two tenants that look identical in every log and report`,
			mutate:   func(r *eventRequest) { r.TenantID = "acme " },
			wantCode: codeInvalid,
		},
		{
			name:     "oversized tenant_id",
			proves:   "an unbounded string is an unbounded write into an indexed column",
			mutate:   func(r *eventRequest) { r.TenantID = strings.Repeat("a", maxFieldLen+1) },
			wantCode: codeInvalid,
		},
		{
			name:     "missing quantity",
			proves:   "an empty quantity reaches Postgres as invalid numeric syntax, which names no field",
			mutate:   func(r *eventRequest) { r.Quantity = "" },
			wantCode: codeInvalid,
		},
		{
			name:     "negative quantity",
			proves:   "a credit is a ledger adjustment with its own audit trail, not usage with a minus sign",
			mutate:   func(r *eventRequest) { r.Quantity = "-1" },
			wantCode: codeInvalid,
		},
		{
			name:     "quantity in scientific notation",
			proves:   "accepting 1e5 would invite clients to send a float's string form, reopening the precision hole",
			mutate:   func(r *eventRequest) { r.Quantity = "1e5" },
			wantCode: codeInvalid,
		},
		{
			name:     "quantity that is not a number",
			proves:   "arbitrary text is rejected before it becomes a database error",
			mutate:   func(r *eventRequest) { r.Quantity = "many" },
			wantCode: codeInvalid,
		},
		{
			name:     "quantity with no digit before the point",
			proves:   `".5" is ambiguous enough to be a client bug, so it is refused rather than guessed at`,
			mutate:   func(r *eventRequest) { r.Quantity = ".5" },
			wantCode: codeInvalid,
		},
		{
			name:     "quantity with a trailing point",
			proves:   `"1." is a truncated number, which usually means the client built the string wrong`,
			mutate:   func(r *eventRequest) { r.Quantity = "1." },
			wantCode: codeInvalid,
		},
		{
			name:   "quantity with leading zeros",
			proves: "leading zeros do not count toward precision, so a padded number is accepted as Postgres would accept it",
			mutate: func(r *eventRequest) { r.Quantity = "000001.5" },
		},
		{
			name:   "quantity at the exact precision limit",
			proves: "the boundary itself is accepted -- NUMERIC(38,9) means 29 integer digits and 9 fractional are legal",
			mutate: func(r *eventRequest) {
				r.Quantity = strings.Repeat("9", maxIntegerDigits) + "." + strings.Repeat("9", maxFractionDigits)
			},
		},
		{
			name:   "quantity one digit over the integer limit",
			proves: "one past the boundary fails here rather than as a numeric overflow at insert time",
			mutate: func(r *eventRequest) {
				r.Quantity = strings.Repeat("9", maxIntegerDigits+1)
			},
			wantCode: codeInvalid,
		},
		{
			name:   "quantity one digit over the fraction limit",
			proves: "excess fractional digits would be silently rounded by Postgres, changing what the customer is billed",
			mutate: func(r *eventRequest) {
				r.Quantity = "1." + strings.Repeat("9", maxFractionDigits+1)
			},
			wantCode: codeInvalid,
		},
		{
			name:     "missing occurred_at",
			proves:   "the zero time is what encoding/json leaves for an absent field, so this is the required-field check too",
			mutate:   func(r *eventRequest) { r.OccurredAt = time.Time{} },
			wantCode: codeInvalid,
		},
		{
			name:   "occurred_at slightly in the future",
			proves: "ordinary clock drift between client and server is tolerated rather than rejected",
			mutate: func(r *eventRequest) { r.OccurredAt = testNow.Add(maxClockSkew - time.Minute) },
		},
		{
			name:     "occurred_at far in the future",
			proves:   "a client with a broken clock cannot place usage in a period that has not started",
			mutate:   func(r *eventRequest) { r.OccurredAt = testNow.Add(24 * time.Hour) },
			wantCode: codeInvalid,
		},
		{
			name:   "occurred_at just inside the backfill window",
			proves: "a genuinely late event within the window is still accepted, per ADR-0001 section 5",
			mutate: func(r *eventRequest) { r.OccurredAt = testNow.Add(-maxBackfillAge + time.Hour) },
		},
		{
			name:     "occurred_at beyond the backfill window",
			proves:   "past the horizon this is a backfill, which must be a human-initiated operation and gets its own code",
			mutate:   func(r *eventRequest) { r.OccurredAt = testNow.Add(-maxBackfillAge - time.Hour) },
			wantCode: codeTooOld,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest()
			tc.mutate(&req)

			got := validate(&req, testNow)

			if tc.wantCode == "" {
				if got != nil {
					t.Fatalf("expected the request to be accepted, got %q (%s)\nthis case proves: %s", got.code, got.detail, tc.proves)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected rejection %q, got acceptance\nthis case proves: %s", tc.wantCode, tc.proves)
			}
			if got.code != tc.wantCode {
				t.Fatalf("expected code %q, got %q (%s)\nthis case proves: %s", tc.wantCode, got.code, got.detail, tc.proves)
			}
		})
	}
}

// The backfill rejection carries a different status from an ordinary bad
// field, because "your request was malformed" and "this usage is too old to
// bill automatically" call for completely different client behaviour. A client
// that cannot tell them apart will retry the one it should escalate.
func TestTooOldUsesADistinctStatus(t *testing.T) {
	req := validRequest()
	req.OccurredAt = testNow.Add(-maxBackfillAge - time.Hour)

	rej := validate(&req, testNow)
	if rej == nil {
		t.Fatal("expected a rejection")
	}
	if rej.status != 422 {
		t.Errorf("too-old status = %d, want 422", rej.status)
	}

	req.Quantity = "not a number"
	req.OccurredAt = testNow.Add(-time.Hour)
	if rej := validate(&req, testNow); rej == nil || rej.status != 400 {
		t.Errorf("invalid-field status = %v, want 400", rej)
	}
}
