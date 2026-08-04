package ingest

import (
	"bytes"
	"testing"
	"time"
)

func TestFingerprint(t *testing.T) {
	base := validRequest()

	tests := []struct {
		name   string
		proves string
		mutate func(*eventRequest)
		want   bool // true if the fingerprint should match the base request
	}{
		{
			name:   "identical request",
			proves: "the hash is deterministic, without which every retry would look like a reused key",
			mutate: func(r *eventRequest) {},
			want:   true,
		},
		{
			name:   "different idempotency_key",
			proves: "the key is the lookup, not part of the content being compared",
			mutate: func(r *eventRequest) { r.IdempotencyKey = "some-other-key" },
			want:   true,
		},
		{
			name:   "quantity written as 1.0 instead of 1",
			proves: "a client that formats the same number differently between attempts is not accused of reuse",
			mutate: func(r *eventRequest) { r.Quantity = "1.0" },
			want:   true,
		},
		{
			name:   "quantity with leading and trailing zeros",
			proves: "canonicalisation strips every formatting difference that does not change the value",
			mutate: func(r *eventRequest) { r.Quantity = "0001.000" },
			want:   true,
		},
		{
			name:   "occurred_at in a different timezone offset",
			proves: "the same instant expressed in another offset is the same event",
			mutate: func(r *eventRequest) {
				r.OccurredAt = r.OccurredAt.In(time.FixedZone("CEST", 2*60*60))
			},
			want: true,
		},
		{
			name:   "different quantity",
			proves: "the amount billed is part of the content, so changing it is a different payload",
			mutate: func(r *eventRequest) { r.Quantity = "2" },
			want:   false,
		},
		{
			name:   "quantity differing below the precision limit",
			proves: "a difference Postgres would store is a difference the fingerprint must see",
			mutate: func(r *eventRequest) { r.Quantity = "1.000000001" },
			want:   false,
		},
		{
			name:   "different meter",
			proves: "the same quantity against another meter is different usage",
			mutate: func(r *eventRequest) { r.Meter = "gb_egress" },
			want:   false,
		},
		{
			name:   "different tenant",
			proves: "content is scoped to the tenant being billed",
			mutate: func(r *eventRequest) { r.TenantID = "globex" },
			want:   false,
		},
		{
			name:   "different occurred_at",
			proves: "event time decides the billing period, so it is part of what the event is",
			mutate: func(r *eventRequest) { r.OccurredAt = r.OccurredAt.Add(-time.Hour) },
			want:   false,
		},
	}

	want := fingerprint(&base)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := validRequest()
			tc.mutate(&req)

			got := bytes.Equal(fingerprint(&req), want)
			if got != tc.want {
				t.Errorf("fingerprints equal = %v, want %v\nthis case proves: %s", got, tc.want, tc.proves)
			}
		})
	}
}

// The reason every field is length-prefixed instead of concatenated.
//
// Plain concatenation makes tenant "ab" + meter "c" indistinguishable from
// tenant "a" + meter "bc" -- both produce "abc". Two genuinely different
// events would then share a fingerprint, and a reused key carrying one of them
// would be waved through as a duplicate.
func TestFingerprintIsUnambiguousAcrossFieldBoundaries(t *testing.T) {
	first := validRequest()
	first.TenantID = "ab"
	first.Meter = "c"

	second := validRequest()
	second.TenantID = "a"
	second.Meter = "bc"

	if bytes.Equal(fingerprint(&first), fingerprint(&second)) {
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
			if got := canonicalQuantity(tc.in); got != tc.want {
				t.Errorf("canonicalQuantity(%q) = %q, want %q\nthis case proves: %s",
					tc.in, got, tc.want, tc.proves)
			}
		})
	}
}
