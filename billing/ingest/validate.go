package ingest

import (
	"fmt"
	"strings"
	"time"
)

// Validation bounds. These exist so that a malformed or hostile request fails
// here, with a clear message, rather than deep inside Postgres with a message
// about numeric syntax that says nothing about which field was wrong.
const (
	// Generous, but bounded. Without a limit an unvalidated field is an
	// unbounded write into an indexed column.
	maxFieldLen = 255

	// NUMERIC(38,9) from migration 000001: 38 total digits, 9 fractional,
	// leaving 29 for the integer part.
	maxIntegerDigits  = 29
	maxFractionDigits = 9

	// occurred_at is client-supplied, so a client with a wrong clock can place
	// usage in the wrong period or manufacture future-dated events. A small
	// forward tolerance absorbs ordinary clock drift without accepting usage
	// that has not happened yet. Closes ADR-0001's clock-skew open question.
	maxClockSkew = 5 * time.Minute

	// ADR-0001 section 5: past this horizon an event is a backfill, and a
	// backfill should be an explicit human-initiated operation rather than
	// something that quietly lands on a customer's bill two months later.
	maxBackfillAge = 35 * 24 * time.Hour
)

// Stable, machine-readable rejection codes. Clients branch on these; the
// human-readable detail is free to change, these are not.
const (
	codeMalformed = "malformed_request"
	codeInvalid   = "invalid_field"
	codeTooOld    = "event_too_old"
)

// rejection is a refusal to accept an event, carrying everything needed to
// report it consistently.
type rejection struct {
	status int
	code   string
	detail string
}

func (r *rejection) Error() string {
	return fmt.Sprintf("%s: %s", r.code, r.detail)
}

func invalid(field, reason string) *rejection {
	return &rejection{
		status: 400,
		code:   codeInvalid,
		detail: fmt.Sprintf("%s %s", field, reason),
	}
}

// validate checks a decoded request against everything we can know without
// touching the database.
//
// now is a parameter rather than a call to time.Now() so that the time-based
// rules are testable at fixed instants. Invariant I5 in PLAN.md — determinism —
// starts with not reading the clock in the middle of business logic.
func validate(req *eventRequest, now time.Time) *rejection {
	if err := validateString("tenant_id", req.TenantID); err != nil {
		return err
	}
	if err := validateString("meter", req.Meter); err != nil {
		return err
	}
	if err := validateString("idempotency_key", req.IdempotencyKey); err != nil {
		return err
	}
	if err := validateQuantity(req.Quantity); err != nil {
		return err
	}
	return validateOccurredAt(req.OccurredAt, now)
}

func validateString(field, value string) *rejection {
	if value == "" {
		return invalid(field, "is required")
	}
	if len(value) > maxFieldLen {
		return invalid(field, fmt.Sprintf("must be at most %d bytes", maxFieldLen))
	}
	// A field arriving with surrounding whitespace is almost always a client
	// bug, and silently accepting it means "acme" and "acme " become two
	// different tenants that look identical in every log and report.
	if strings.TrimSpace(value) != value {
		return invalid(field, "must not have leading or trailing whitespace")
	}
	return nil
}

// validateQuantity checks that the string is a plain decimal that Postgres will
// accept into NUMERIC(38,9).
//
// Hand-rolled rather than using a decimal library, because all we need is
// validation of a string we then pass through untouched. Parsing it into a Go
// numeric type would mean picking a representation, and the one obvious choice
// (float64) is precisely what ADR-0001 section 3 forbids.
func validateQuantity(value string) *rejection {
	if value == "" {
		return invalid("quantity", "is required")
	}

	// Scientific notation ("1e5") is valid input to Postgres NUMERIC but is
	// rejected here: it is never what a metering client means to send, and
	// accepting it invites a client to send a float's string form.
	if strings.ContainsAny(value, "eE") {
		return invalid("quantity", "must not use scientific notation")
	}

	// Negative usage is refused. A credit or correction is a real concept but
	// it is a ledger adjustment with its own audit trail, not a usage event
	// with a minus sign. Revisit if and when credits are designed.
	if strings.HasPrefix(value, "-") {
		return invalid("quantity", "must not be negative")
	}
	if strings.HasPrefix(value, "+") {
		return invalid("quantity", "must not have a leading plus sign")
	}

	integer, fraction, hasPoint := strings.Cut(value, ".")
	if integer == "" {
		return invalid("quantity", "must have at least one digit before the decimal point")
	}
	if hasPoint && fraction == "" {
		return invalid("quantity", "must have at least one digit after the decimal point")
	}
	if !allDigits(integer) || (hasPoint && !allDigits(fraction)) {
		return invalid("quantity", "must be a decimal number")
	}

	// Leading zeros are stripped before counting, so "000001" is one digit and
	// not six. Postgres would accept it; rejecting it would be surprising.
	if len(strings.TrimLeft(integer, "0")) > maxIntegerDigits {
		return invalid("quantity", fmt.Sprintf("must have at most %d digits before the decimal point", maxIntegerDigits))
	}
	if len(fraction) > maxFractionDigits {
		return invalid("quantity", fmt.Sprintf("must have at most %d digits after the decimal point", maxFractionDigits))
	}
	return nil
}

func validateOccurredAt(occurredAt, now time.Time) *rejection {
	// The zero value is what encoding/json leaves behind when the field is
	// absent, so this doubles as the required-field check.
	if occurredAt.IsZero() {
		return invalid("occurred_at", "is required")
	}
	if occurredAt.After(now.Add(maxClockSkew)) {
		return invalid("occurred_at", "must not be in the future")
	}

	// CRITICAL: this is the backfill boundary from ADR-0001 section 5, and it
	// gets its own status and code rather than folding into invalid_field.
	// "Your request was malformed" and "this usage is too old to bill
	// automatically, use the backfill path" call for completely different
	// client behaviour, and a client that cannot tell them apart will retry
	// the one it should escalate.
	if occurredAt.Before(now.Add(-maxBackfillAge)) {
		return &rejection{
			status: 422,
			code:   codeTooOld,
			detail: fmt.Sprintf("occurred_at is more than %d days old; use the backfill path", int(maxBackfillAge.Hours()/24)),
		}
	}
	return nil
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}
