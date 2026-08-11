package ledger

import (
	"errors"
	"fmt"
	"strings"
)

// Scale is the number of decimal places an Amount carries.
//
// Six, not two. Usage billing prices things below a cent -- $0.0001 per API
// call is ordinary -- so a ledger denominated in cents would round every
// individual posting to zero and bill nothing. Rounding to a currency's minor
// unit is a presentation decision that belongs at invoice time, not a storage
// decision that silently destroys the numbers on the way in.
//
// Six places is also what Google Ads and most usage-billing systems settle on,
// usually called "micros".
const Scale = 6

// scaleFactor is 10^Scale: how many micro-units make one whole unit.
const scaleFactor = 1_000_000

// Amount is a quantity of money in micro-units, as a count rather than a
// fraction.
//
// An integer, never a float. ADR-0001 section 3 forbids float for quantities
// and the same reasoning applies with more force here: binary floating point
// cannot represent most decimal fractions, so summing millions of postings
// accumulates error until the ledger fails to balance by amounts nobody can
// account for. An integer count of a fixed unit is exact by construction.
//
// int64 holds roughly ±9.2 x 10^12 whole units at this scale, which is far
// beyond any plausible tenant balance.
type Amount int64

var (
	// ErrOverflow means an arithmetic result does not fit in an Amount.
	ErrOverflow = errors.New("ledger: amount overflow")

	// ErrMalformedAmount means a string could not be read as an amount.
	ErrMalformedAmount = errors.New("ledger: malformed amount")
)

// Add returns a+b, refusing to wrap around.
//
// CRITICAL: silent integer overflow in a ledger is the worst available failure.
// Wrapping turns a large positive balance into a large negative one, the books
// still balance because both sides wrapped consistently, and every downstream
// check passes while the number is nonsense. Go does not trap overflow, so it
// has to be detected here.
//
// The test is direction-based: adding a positive number must not make the
// result smaller, and adding a negative one must not make it larger.
func (a Amount) Add(b Amount) (Amount, error) {
	sum := a + b
	if (b > 0 && sum < a) || (b < 0 && sum > a) {
		return 0, fmt.Errorf("%w: %d + %d", ErrOverflow, a, b)
	}
	return sum, nil
}

// Neg returns -a.
func (a Amount) Neg() (Amount, error) {
	// The most negative int64 has no positive counterpart, so negating it
	// overflows. Every other value is safe.
	if a == -1<<63 {
		return 0, fmt.Errorf("%w: negating %d", ErrOverflow, a)
	}
	return -a, nil
}

// String renders the amount as a decimal with exactly Scale places.
//
// Fixed places rather than trimmed: in money, "1.5" and "1.500000" are the same
// value but not the same statement, and a ledger that varies its formatting
// makes diffs between two reports unreadable.
func (a Amount) String() string {
	negative := a < 0

	// Taking the absolute value first keeps the digit arithmetic away from
	// negative-number division, whose rounding direction in Go is toward zero
	// and would misplace the decimal point.
	units := int64(a)
	if negative {
		// Guard the most negative value, which has no positive counterpart.
		if units == -1<<63 {
			return "-9223372036854.775808"
		}
		units = -units
	}

	whole := units / scaleFactor
	fraction := units % scaleFactor

	sign := ""
	if negative {
		sign = "-"
	}
	return fmt.Sprintf("%s%d.%0*d", sign, whole, Scale, fraction)
}

// ParseAmount reads a decimal string into an Amount.
//
// Rejects anything with more than Scale decimal places rather than rounding it.
// Rounding here would be a silent change to a monetary value at the boundary of
// the system, which is exactly where it would never be noticed.
func ParseAmount(s string) (Amount, error) {
	if s == "" {
		return 0, fmt.Errorf("%w: empty", ErrMalformedAmount)
	}

	negative := false
	body := s
	if rest, ok := strings.CutPrefix(body, "-"); ok {
		negative = true
		body = rest
	}
	if body == "" {
		return 0, fmt.Errorf("%w: %q has a sign and no digits", ErrMalformedAmount, s)
	}

	integer, fraction, hasPoint := strings.Cut(body, ".")
	if integer == "" {
		return 0, fmt.Errorf("%w: %q has no digit before the decimal point", ErrMalformedAmount, s)
	}
	if hasPoint && fraction == "" {
		return 0, fmt.Errorf("%w: %q has no digit after the decimal point", ErrMalformedAmount, s)
	}
	if len(fraction) > Scale {
		return 0, fmt.Errorf("%w: %q has more than %d decimal places", ErrMalformedAmount, s, Scale)
	}

	whole, err := parseDigits(integer)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", err, s)
	}

	// Pad so "1.5" and "1.500000" mean the same number of micro-units.
	padded := fraction + strings.Repeat("0", Scale-len(fraction))
	micros, err := parseDigits(padded)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", err, s)
	}

	// Checked rather than assumed: a 19-digit integer part would wrap here.
	scaled := whole * scaleFactor
	if whole != 0 && scaled/scaleFactor != whole {
		return 0, fmt.Errorf("%w: %q is too large", ErrOverflow, s)
	}
	total := scaled + micros
	if total < scaled {
		return 0, fmt.Errorf("%w: %q is too large", ErrOverflow, s)
	}

	if negative {
		return Amount(-total), nil
	}
	return Amount(total), nil
}

// parseDigits converts a run of ASCII digits, rejecting anything else.
//
// Hand-rolled rather than strconv.ParseInt because that accepts signs,
// underscores, and leading whitespace -- all of which would let a malformed
// monetary string through as a plausible number.
func parseDigits(s string) (int64, error) {
	if s == "" {
		return 0, ErrMalformedAmount
	}
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, ErrMalformedAmount
		}
		next := n*10 + int64(r-'0')
		if next < n {
			return 0, ErrOverflow
		}
		n = next
	}
	return n, nil
}
