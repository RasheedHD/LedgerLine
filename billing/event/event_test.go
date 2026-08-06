package event

import (
	"bytes"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func validEvent() UsageEvent {
	e := UsageEvent{
		TenantID:       "acme",
		Meter:          "api_calls",
		Quantity:       "1",
		OccurredAt:     testNow.Add(-time.Hour),
		ReceivedAt:     testNow,
		IdempotencyKey: "key-1",
	}
	e.Fingerprint = Fingerprint(&e)
	return e
}

// An event must survive the log unchanged. Every field is billable or decides
// where the event belongs, so any of them silently changing shape is a wrong
// invoice.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	want := validEvent()

	record, err := Encode(&want)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	got, err := Decode(record)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.TenantID != want.TenantID || got.Meter != want.Meter {
		t.Errorf("identity fields changed: %+v", got)
	}
	if got.Quantity != want.Quantity {
		t.Errorf("quantity = %q, want %q -- the decimal string must survive verbatim", got.Quantity, want.Quantity)
	}
	if !got.OccurredAt.Equal(want.OccurredAt) {
		t.Errorf("occurred_at = %s, want %s", got.OccurredAt, want.OccurredAt)
	}
	if !got.ReceivedAt.Equal(want.ReceivedAt) {
		t.Errorf("received_at = %s, want %s", got.ReceivedAt, want.ReceivedAt)
	}
	if got.IdempotencyKey != want.IdempotencyKey {
		t.Errorf("idempotency_key = %q, want %q", got.IdempotencyKey, want.IdempotencyKey)
	}
	if !bytes.Equal(got.Fingerprint, want.Fingerprint) {
		t.Error("fingerprint did not survive the round trip")
	}
}

// Invariant I5: replay must be deterministic. If encoding the same event twice
// produced different bytes, a log replay could not be compared against the
// original and the whole audit story falls apart.
func TestEncodingIsDeterministic(t *testing.T) {
	e := validEvent()

	first, err := Encode(&e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := Encode(&e)
		if err != nil {
			t.Fatalf("encode %d: %v", i, err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("encoding is not deterministic; run %d differs", i)
		}
	}
}

// A quantity is never a JSON number on the wire. If it were, decoding would go
// through float64 and lose precision that NUMERIC(38,9) exists to keep.
func TestQuantityIsEncodedAsAString(t *testing.T) {
	e := validEvent()
	e.Quantity = "0.000000001"

	record, err := Encode(&e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Contains(record, []byte(`"quantity":"0.000000001"`)) {
		t.Fatalf("quantity is not a quoted string in %s", record)
	}

	got, err := Decode(record)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Quantity != "0.000000001" {
		t.Errorf("quantity = %q after round trip, want 0.000000001", got.Quantity)
	}
}

// A record carrying a field this build does not know about is refused rather
// than processed with data missing. Quietly dropping a billable field written
// by a newer build is the failure worth being strict about.
func TestDecodeRejectsUnknownFields(t *testing.T) {
	record := []byte(`{"tenant_id":"acme","meter":"api_calls","quantity":"1",` +
		`"occurred_at":"2026-08-03T11:00:00Z","received_at":"2026-08-03T12:00:00Z",` +
		`"idempotency_key":"key-1","discount_code":"HALFPRICE"}`)

	if _, err := Decode(record); err == nil {
		t.Fatal("decoded a record with an unknown field; a newer build's data would be silently dropped")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Fatal("decoded a malformed record")
	}
}

func TestFingerprint(t *testing.T) {
	tests := []struct {
		name   string
		proves string
		mutate func(*UsageEvent)
		want   bool // true if the fingerprint should match the base event
	}{
		{
			name:   "identical event",
			proves: "the hash is deterministic, without which every retry would look like a reused key",
			mutate: func(e *UsageEvent) {},
			want:   true,
		},
		{
			name:   "different idempotency_key",
			proves: "the key is the lookup, not part of the content being compared",
			mutate: func(e *UsageEvent) { e.IdempotencyKey = "some-other-key" },
			want:   true,
		},
		{
			name:   "different received_at",
			proves: "ingest time differs on every attempt, so including it would make every retry look new",
			mutate: func(e *UsageEvent) { e.ReceivedAt = e.ReceivedAt.Add(time.Hour) },
			want:   true,
		},
		{
			name:   "quantity written as 1.0 instead of 1",
			proves: "a client that formats the same number differently between attempts is not accused of reuse",
			mutate: func(e *UsageEvent) { e.Quantity = "1.0" },
			want:   true,
		},
		{
			name:   "quantity with leading and trailing zeros",
			proves: "canonicalisation strips every formatting difference that does not change the value",
			mutate: func(e *UsageEvent) { e.Quantity = "0001.000" },
			want:   true,
		},
		{
			name:   "occurred_at in a different timezone offset",
			proves: "the same instant expressed in another offset is the same event",
			mutate: func(e *UsageEvent) { e.OccurredAt = e.OccurredAt.In(time.FixedZone("CEST", 2*60*60)) },
			want:   true,
		},
		{
			name:   "different quantity",
			proves: "the amount billed is part of the content, so changing it is a different payload",
			mutate: func(e *UsageEvent) { e.Quantity = "2" },
			want:   false,
		},
		{
			name:   "quantity differing below the precision limit",
			proves: "a difference Postgres would store is a difference the fingerprint must see",
			mutate: func(e *UsageEvent) { e.Quantity = "1.000000001" },
			want:   false,
		},
		{
			name:   "different meter",
			proves: "the same quantity against another meter is different usage",
			mutate: func(e *UsageEvent) { e.Meter = "gb_egress" },
			want:   false,
		},
		{
			name:   "different tenant",
			proves: "content is scoped to the tenant being billed",
			mutate: func(e *UsageEvent) { e.TenantID = "globex" },
			want:   false,
		},
		{
			name:   "different occurred_at",
			proves: "event time decides the billing period, so it is part of what the event is",
			mutate: func(e *UsageEvent) { e.OccurredAt = e.OccurredAt.Add(-time.Hour) },
			want:   false,
		},
	}

	base := validEvent()
	want := Fingerprint(&base)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEvent()
			tc.mutate(&e)

			got := bytes.Equal(Fingerprint(&e), want)
			if got != tc.want {
				t.Errorf("fingerprints equal = %v, want %v\nthis case proves: %s", got, tc.want, tc.proves)
			}
		})
	}
}

// The reason every field is length-prefixed instead of concatenated.
//
// Plain concatenation makes tenant "ab" + meter "c" indistinguishable from
// tenant "a" + meter "bc" -- both produce "abc". Two genuinely different events
// would then share a fingerprint, and a reused key carrying one of them would
// be waved through as a duplicate.
func TestFingerprintIsUnambiguousAcrossFieldBoundaries(t *testing.T) {
	first := validEvent()
	first.TenantID = "ab"
	first.Meter = "c"

	second := validEvent()
	second.TenantID = "a"
	second.Meter = "bc"

	if bytes.Equal(Fingerprint(&first), Fingerprint(&second)) {
		t.Error("field boundaries are ambiguous: two different events share a fingerprint")
	}
}

func TestCanonicalQuantity(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		proves string
	}{
		{"1", "1", "an already-canonical integer is untouched"},
		{"1.0", "1", "a trailing zero fraction collapses to the integer"},
		{"1.500", "1.5", "trailing zeros are stripped but significant digits are not"},
		{"0001", "1", "leading zeros carry no value"},
		{"0001.000", "1", "leading and trailing zeros are stripped together"},
		{"0", "0", "zero survives having all its zeros stripped"},
		{"0.0", "0", "a zero written with a fraction is still zero"},
		{"0.000000001", "0.000000001", "the smallest storable value keeps every digit"},
		{"10", "10", "a zero that carries magnitude is not stripped"},
		{"100.100", "100.1", "interior zeros are preserved on both sides"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := CanonicalQuantity(tc.in); got != tc.want {
				t.Errorf("CanonicalQuantity(%q) = %q, want %q\nthis case proves: %s",
					tc.in, got, tc.want, tc.proves)
			}
		})
	}
}
