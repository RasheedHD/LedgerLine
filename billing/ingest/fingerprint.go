package ingest

import (
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"strings"
	"time"
)

// fingerprint returns a hash over the billable content of an event, used to
// detect an idempotency key reused for different usage. See ADR-0005.
//
// What goes in is exactly the fields that determine what the customer is
// charged. What stays out matters just as much:
//
//   - received_at is ours and differs on every attempt, so including it would
//     make every retry look like a different payload.
//   - idempotency_key is the lookup key, not part of the content being
//     compared.
//
// SHA-256 rather than a cheaper non-cryptographic hash: this is a correctness
// check where a collision means silently accepting divergent usage as a
// duplicate, and the cost is irrelevant next to the database round trip that
// follows.
func fingerprint(req *eventRequest) []byte {
	h := sha256.New()
	writeField(h, req.TenantID)
	writeField(h, req.Meter)
	writeField(h, canonicalQuantity(req.Quantity))

	// Normalised to UTC so that the same instant expressed in different
	// offsets -- "2026-08-03T12:00:00Z" and "2026-08-03T14:00:00+02:00" --
	// produces one fingerprint rather than two.
	writeField(h, req.OccurredAt.UTC().Format(time.RFC3339Nano))

	return h.Sum(nil)
}

// CRITICAL: each field is length-prefixed rather than concatenated or
// delimited.
//
// Plain concatenation is ambiguous: tenant "ab" with meter "c" and tenant "a"
// with meter "bc" both produce "abc" and would hash identically, so two
// genuinely different events could be judged the same payload. A delimiter
// only moves the problem, because any byte chosen can also appear inside a
// field. Writing the length first makes the encoding unambiguous regardless of
// content.
func writeField(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))

	// hash.Hash documents that Write never returns an error, so there is
	// nothing to handle here.
	h.Write(length[:])
	h.Write([]byte(value))
}

// canonicalQuantity strips formatting that does not change the value, so a
// client that sends "1" on the first attempt and "1.0" on the retry is not
// accused of reusing its key for different usage.
//
// Purely string manipulation. Parsing into a numeric type to normalise would
// mean choosing a representation, and the obvious one is exactly what
// ADR-0001 section 3 forbids.
//
// Safe to assume a well-formed input here: validate() has already established
// that this is digits with at most one decimal point and no sign.
func canonicalQuantity(quantity string) string {
	integer, fraction, hasPoint := strings.Cut(quantity, ".")

	// "000001" and "1" are the same number.
	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	if !hasPoint {
		return integer
	}

	// "1.500" and "1.5" are the same number; "1.000" and "1" are too.
	fraction = strings.TrimRight(fraction, "0")
	if fraction == "" {
		return integer
	}
	return integer + "." + fraction
}
