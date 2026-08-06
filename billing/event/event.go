// Package event defines the usage event as it travels between components.
//
// This is the seam ADR-0001 describes: the shape ingest writes to the broker
// log and the consumer reads back out of it. It lives in its own package so
// that neither side owns it, and so the encoding is defined exactly once.
package event

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash"
	"strings"
	"time"
)

// UsageEvent is one metered event.
//
// Field order is deliberate and load-bearing: encoding/json marshals struct
// fields in declaration order, so reordering them changes the bytes written to
// the log. Invariant I5 -- replay produces a byte-identical result -- depends
// on this encoding being stable, so this struct is not a place to tidy.
type UsageEvent struct {
	TenantID string `json:"tenant_id"`
	Meter    string `json:"meter"`

	// A decimal string, never a number. ADR-0001 section 3: JSON numbers
	// unmarshal into float64 in Go, which would reintroduce exactly the
	// precision loss NUMERIC exists to prevent -- and it would happen here,
	// silently, on every round trip through the log.
	Quantity string `json:"quantity"`

	// Event time: when the usage happened. Client-supplied, decides the
	// billing period.
	OccurredAt time.Time `json:"occurred_at"`

	// Ingest time: when we took durable custody. Ours.
	ReceivedAt time.Time `json:"received_at"`

	IdempotencyKey string `json:"idempotency_key"`

	// Carried rather than recomputed downstream, so the value the consumer
	// stores is exactly the one ingest hashed. Recomputing would work today
	// and drift the moment the two sides disagree about canonicalisation.
	Fingerprint []byte `json:"fingerprint"`
}

// Encode returns the event's log record representation.
//
// JSON rather than a compact binary format. It costs space and speed, and buys
// something worth more at this stage: a log you can read with `cat` when a
// billing discrepancy needs explaining. A binary format is the obvious later
// optimisation, and the record framing in broker/log is already agnostic to
// what is inside it.
func Encode(e *UsageEvent) ([]byte, error) {
	encoded, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("encode event: %w", err)
	}
	return encoded, nil
}

// Decode parses a log record back into an event.
func Decode(record []byte) (*UsageEvent, error) {
	var e UsageEvent

	// Unknown fields are rejected so that a record written by a newer build,
	// carrying a field this one does not understand, fails loudly instead of
	// being silently processed with data missing. Losing a billable field
	// quietly is the failure mode worth spending strictness on.
	dec := json.NewDecoder(strings.NewReader(string(record)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("decode event: %w", err)
	}
	return &e, nil
}

// Fingerprint hashes the billable content of an event, used to detect an
// idempotency key reused for different usage. See ADR-0005.
//
// What stays out matters as much as what goes in: ReceivedAt differs on every
// attempt, and IdempotencyKey is the lookup rather than part of the content
// being compared against it.
func Fingerprint(e *UsageEvent) []byte {
	h := sha256.New()
	writeField(h, e.TenantID)
	writeField(h, e.Meter)
	writeField(h, CanonicalQuantity(e.Quantity))

	// Normalised to UTC so the same instant in different offsets produces one
	// fingerprint.
	writeField(h, e.OccurredAt.UTC().Format(time.RFC3339Nano))

	return h.Sum(nil)
}

// CRITICAL: each field is length-prefixed rather than concatenated.
//
// Plain concatenation is ambiguous: tenant "ab" with meter "c" and tenant "a"
// with meter "bc" both produce "abc" and would hash identically, so two
// genuinely different events could be judged the same payload. A delimiter only
// moves the problem, since any byte chosen can also appear inside a field.
func writeField(h hash.Hash, value string) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))

	// hash.Hash documents that Write never returns an error.
	h.Write(length[:])
	h.Write([]byte(value))
}

// CanonicalQuantity strips formatting that does not change the value, so a
// client sending "1" then "1.0" on its retry is not accused of reusing its key
// for different usage.
//
// Pure string manipulation. Parsing into a numeric type to normalise would mean
// choosing a representation, and the obvious one is what ADR-0001 section 3
// forbids.
//
// Assumes a well-formed input: validation has already established digits with
// at most one decimal point and no sign.
func CanonicalQuantity(quantity string) string {
	integer, fraction, hasPoint := strings.Cut(quantity, ".")

	integer = strings.TrimLeft(integer, "0")
	if integer == "" {
		integer = "0"
	}
	if !hasPoint {
		return integer
	}

	fraction = strings.TrimRight(fraction, "0")
	if fraction == "" {
		return integer
	}
	return integer + "." + fraction
}
