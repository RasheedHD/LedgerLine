package ledger

import (
	"errors"
	"math/rand"
	"testing"
)

func TestParseAmount(t *testing.T) {
	tests := []struct {
		in      string
		want    Amount
		wantErr error
		proves  string
	}{
		{in: "0", want: 0, proves: "zero is a legal amount"},
		{in: "1", want: 1_000_000, proves: "a whole unit is scaled to micro-units"},
		{in: "1.5", want: 1_500_000, proves: "a short fraction is padded, so 1.5 and 1.500000 agree"},
		{in: "1.500000", want: 1_500_000, proves: "a fully written fraction gives the same value"},
		{in: "0.000001", want: 1, proves: "the smallest representable amount survives"},
		{in: "-1.25", want: -1_250_000, proves: "negative amounts parse, since credits are stored signed"},
		{in: "0.0001", want: 100, proves: "a sub-cent price is representable, which is why the scale is 6 and not 2"},
		{in: "1234567.891011", want: 1_234_567_891_011, proves: "a large amount with full precision is exact"},

		{in: "", wantErr: ErrMalformedAmount, proves: "an empty string is not zero"},
		{in: "-", wantErr: ErrMalformedAmount, proves: "a sign with no digits is refused"},
		{in: ".5", wantErr: ErrMalformedAmount, proves: "a missing integer part is ambiguous enough to be a bug"},
		{in: "1.", wantErr: ErrMalformedAmount, proves: "a truncated decimal usually means the string was built wrong"},
		{in: "1.2.3", wantErr: ErrMalformedAmount, proves: "two decimal points is not a number"},
		{in: "abc", wantErr: ErrMalformedAmount, proves: "text is refused rather than treated as zero"},
		{in: "1e6", wantErr: ErrMalformedAmount, proves: "scientific notation is how a float's string form sneaks in"},
		{in: " 1", wantErr: ErrMalformedAmount, proves: "strconv would accept surrounding space; money should not"},
		{in: "+1", wantErr: ErrMalformedAmount, proves: "one spelling per value keeps postings readable"},
		{in: "1_000", wantErr: ErrMalformedAmount, proves: "Go's underscore separators are not decimal syntax"},

		{
			in:      "1.0000001",
			wantErr: ErrMalformedAmount,
			proves:  "more precision than we store is REFUSED, not rounded -- silently rounding money at the boundary is where it would never be noticed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseAmount(tc.in)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v\nthis case proves: %s", err, tc.wantErr, tc.proves)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error %v\nthis case proves: %s", err, tc.proves)
			}
			if got != tc.want {
				t.Errorf("= %d, want %d\nthis case proves: %s", got, tc.want, tc.proves)
			}
		})
	}
}

func TestAmountString(t *testing.T) {
	tests := []struct {
		in     Amount
		want   string
		proves string
	}{
		{0, "0.000000", "zero is written at full scale, so columns line up"},
		{1_000_000, "1.000000", "a whole unit always shows its decimals"},
		{1_500_000, "1.500000", "trailing zeros are kept; in money the precision is part of the statement"},
		{1, "0.000001", "the smallest unit is not lost in formatting"},
		{-1_250_000, "-1.250000", "the sign goes in front of the whole number, not the fraction"},
		{-1, "-0.000001", "a tiny negative keeps its sign and its magnitude"},
	}

	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.in.String(); got != tc.want {
				t.Errorf("= %q, want %q\nthis case proves: %s", got, tc.want, tc.proves)
			}
		})
	}
}

// Parsing and formatting must be exact inverses. If they were not, an amount
// would change every time it passed through a report and back.
func TestParseStringRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	for i := 0; i < 5000; i++ {
		want := Amount(rng.Int63n(1_000_000_000_000) - 500_000_000_000)

		got, err := ParseAmount(want.String())
		if err != nil {
			t.Fatalf("ParseAmount(%q): %v", want.String(), err)
		}
		if got != want {
			t.Fatalf("round trip changed %d into %d (via %q)", want, got, want.String())
		}
	}
}

// CRITICAL behaviour: addition must refuse to wrap.
//
// Go does not trap integer overflow. A wrapped total turns a large positive
// balance into a large negative one, and because both sides of the ledger wrap
// consistently the books still balance -- every check passes while the numbers
// are nonsense.
func TestAddDetectsOverflow(t *testing.T) {
	max := Amount(1<<63 - 1)
	min := Amount(-1 << 63)

	if _, err := max.Add(1); !errors.Is(err, ErrOverflow) {
		t.Errorf("max+1 error = %v, want ErrOverflow", err)
	}
	if _, err := min.Add(-1); !errors.Is(err, ErrOverflow) {
		t.Errorf("min-1 error = %v, want ErrOverflow", err)
	}

	// Ordinary arithmetic must still work, including near the limits.
	if got, err := max.Add(-1); err != nil || got != max-1 {
		t.Errorf("max-1 = %d, %v; want %d", got, err, max-1)
	}
	if got, err := Amount(5).Add(-7); err != nil || got != -2 {
		t.Errorf("5+(-7) = %d, %v; want -2", got, err)
	}
}

func TestNegDetectsOverflow(t *testing.T) {
	// The most negative int64 has no positive counterpart, so negating it
	// wraps back to itself -- a sign flip that silently does not happen.
	if _, err := Amount(-1 << 63).Neg(); !errors.Is(err, ErrOverflow) {
		t.Errorf("negating the minimum did not report overflow")
	}
	if got, err := Amount(5).Neg(); err != nil || got != -5 {
		t.Errorf("Neg(5) = %d, %v; want -5", got, err)
	}
}

// The reason this type is an integer and not a float, demonstrated rather than
// asserted: a hundred thousand additions of a sub-cent amount must land exactly
// on the arithmetic answer.
func TestRepeatedAdditionIsExact(t *testing.T) {
	const times = 100_000
	tenth := mustAmount(t, "0.0001")

	var total Amount
	for i := 0; i < times; i++ {
		next, err := total.Add(tenth)
		if err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
		total = next
	}

	if want := mustAmount(t, "10"); total != want {
		t.Fatalf("summing %s %d times gave %s, want exactly %s", tenth, times, total, want)
	}
}
