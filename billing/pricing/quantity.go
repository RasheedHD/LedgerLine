package pricing

import (
	"errors"
	"fmt"
	"strings"
)

// QuantityScale is the number of decimal places a Quantity carries.
//
// Nine, matching the NUMERIC(38,9) column usage events are stored in. Anything
// less would mean a quantity that survived ingest could not be priced, which is
// a silent truncation in the worst possible place.
const QuantityScale = 9

const quantityFactor = 1_000_000_000

// Quantity is an amount of usage in nano-units.
//
// An integer count of a fixed unit, for the same reason ledger.Amount is: exact
// by construction, where a float accumulates error that eventually shows up as
// an invoice nobody can reconcile.
//
// int64 at this scale holds roughly 9.2 billion whole units. Generous for API
// calls or gigabytes, and narrower than the NUMERIC(38,9) column it comes
// from -- see D33.
type Quantity int64

var (
	// ErrMalformedQuantity means a string could not be read as a quantity.
	ErrMalformedQuantity = errors.New("pricing: malformed quantity")

	// ErrQuantityOverflow means a value does not fit in a Quantity.
	ErrQuantityOverflow = errors.New("pricing: quantity overflow")
)

// ParseQuantity reads a decimal string into a Quantity.
//
// Refuses more precision than it stores rather than rounding. A quantity is
// what the customer is charged for; quietly dropping digits from it changes the
// bill.
func ParseQuantity(s string) (Quantity, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrMalformedQuantity)
	}
	if strings.HasPrefix(s, "-") {
		// Usage is never negative. ADR-0004 already refuses this at ingest;
		// refused again here because pricing is reachable from elsewhere.
		return 0, fmt.Errorf("%w: %q is negative", ErrMalformedQuantity, s)
	}
	if strings.ContainsAny(s, "eE+_ ") {
		return 0, fmt.Errorf("%w: %q is not a plain decimal", ErrMalformedQuantity, s)
	}

	integer, fraction, hasPoint := strings.Cut(s, ".")
	if integer == "" {
		return 0, fmt.Errorf("%w: %q has no digit before the decimal point", ErrMalformedQuantity, s)
	}
	if hasPoint && fraction == "" {
		return 0, fmt.Errorf("%w: %q has no digit after the decimal point", ErrMalformedQuantity, s)
	}
	if len(fraction) > QuantityScale {
		return 0, fmt.Errorf("%w: %q has more than %d decimal places", ErrMalformedQuantity, s, QuantityScale)
	}

	whole, err := parseQuantityDigits(integer)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", err, s)
	}
	padded := fraction + strings.Repeat("0", QuantityScale-len(fraction))
	nanos, err := parseQuantityDigits(padded)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", err, s)
	}

	scaled := whole * quantityFactor
	if whole != 0 && scaled/quantityFactor != whole {
		return 0, fmt.Errorf("%w: %q is too large", ErrQuantityOverflow, s)
	}
	total := scaled + nanos
	if total < scaled {
		return 0, fmt.Errorf("%w: %q is too large", ErrQuantityOverflow, s)
	}
	return Quantity(total), nil
}

// String renders the quantity as a decimal with exactly QuantityScale places.
func (q Quantity) String() string {
	whole := int64(q) / quantityFactor
	fraction := int64(q) % quantityFactor
	return fmt.Sprintf("%d.%0*d", whole, QuantityScale, fraction)
}

// Add returns q+other, refusing to wrap.
func (q Quantity) Add(other Quantity) (Quantity, error) {
	sum := q + other
	if (other > 0 && sum < q) || (other < 0 && sum > q) {
		return 0, fmt.Errorf("%w: %d + %d", ErrQuantityOverflow, q, other)
	}
	return sum, nil
}

// parseQuantityDigits converts a run of ASCII digits, rejecting anything else.
//
// Hand-rolled rather than strconv.ParseInt, which accepts signs, underscores,
// and surrounding whitespace -- every one of which would let a malformed
// quantity through as a plausible number.
func parseQuantityDigits(s string) (int64, error) {
	if s == "" {
		return 0, ErrMalformedQuantity
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, ErrMalformedQuantity
		}
		next := n*10 + int64(r-'0')
		if next < n {
			return 0, ErrQuantityOverflow
		}
		n = next
	}
	return n, nil
}
