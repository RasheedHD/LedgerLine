package pricing

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/RasheedHD/LedgerLine/billing/ledger"
)

func mustQuantity(t *testing.T, s string) Quantity {
	t.Helper()
	q, err := ParseQuantity(s)
	if err != nil {
		t.Fatalf("ParseQuantity(%q): %v", s, err)
	}
	return q
}

func mustAmount(t *testing.T, s string) ledger.Amount {
	t.Helper()
	a, err := ledger.ParseAmount(s)
	if err != nil {
		t.Fatalf("ParseAmount(%q): %v", s, err)
	}
	return a
}

func upTo(q Quantity) *Quantity { return &q }

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(
		Meter{Name: "api_calls", Unit: "call"},
		Meter{Name: "gb_egress", Unit: "GB"},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

// Graduated tiers charge each portion of the quantity at its own rate. The
// boundaries are where an off-by-one hides, so every case sits exactly at, one
// below, or one above one.
func TestGraduatedTiers(t *testing.T) {
	// First 1000 units at $0.01, next 9000 at $0.005, beyond that $0.001.
	tiers := []Tier{
		{UpTo: upTo(mustQuantity(t, "1000")), UnitPrice: mustAmount(t, "0.01")},
		{UpTo: upTo(mustQuantity(t, "10000")), UnitPrice: mustAmount(t, "0.005")},
		{UpTo: nil, UnitPrice: mustAmount(t, "0.001")},
	}

	tests := []struct {
		name     string
		proves   string
		quantity string
		want     string
	}{
		{
			name:     "zero usage",
			proves:   "no usage costs nothing; a tiered price must not have a hidden minimum",
			quantity: "0",
			want:     "0",
		},
		{
			name:     "one unit",
			proves:   "the smallest billable quantity uses the first tier's rate",
			quantity: "1",
			want:     "0.01",
		},
		{
			name:     "one below the first boundary",
			proves:   "999 units are still entirely in tier one",
			quantity: "999",
			want:     "9.99",
		},
		{
			name:     "exactly at the first boundary",
			proves:   "the bound is INCLUSIVE, so 1000 units are all at tier one's rate",
			quantity: "1000",
			want:     "10",
		},
		{
			name:     "one above the first boundary",
			proves:   "only the excess unit crosses into tier two, not the whole quantity",
			quantity: "1001",
			want:     "10.005",
		},
		{
			name:     "exactly at the second boundary",
			proves:   "1000 at 0.01 plus 9000 at 0.005 -- the split, not a flat rate",
			quantity: "10000",
			want:     "55",
		},
		{
			name:     "one above the second boundary",
			proves:   "the third tier only ever prices what is above 10000",
			quantity: "10001",
			want:     "55.001",
		},
		{
			name:     "crossing every tier",
			proves:   "a large quantity accumulates across all three bands",
			quantity: "100000",
			want:     "145",
		},
		{
			name:     "fractional quantity",
			proves:   "quantities need not be whole; a half unit is charged at half the rate",
			quantity: "0.5",
			want:     "0.005",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyGraduated(tiers, mustQuantity(t, tc.quantity))
			if err != nil {
				t.Fatalf("applyGraduated: %v\nthis case proves: %s", err, tc.proves)
			}
			if want := mustAmount(t, tc.want); got != want {
				t.Errorf("= %s, want %s\nthis case proves: %s", got, want, tc.proves)
			}
		})
	}
}

// Volume pricing charges EVERY unit at the rate of the tier the total lands in,
// so crossing a boundary makes the earlier units cheaper too. That difference
// from graduated is the whole reason both exist.
func TestVolumeTiers(t *testing.T) {
	tiers := []Tier{
		{UpTo: upTo(mustQuantity(t, "1000")), UnitPrice: mustAmount(t, "0.01")},
		{UpTo: upTo(mustQuantity(t, "10000")), UnitPrice: mustAmount(t, "0.005")},
		{UpTo: nil, UnitPrice: mustAmount(t, "0.001")},
	}

	tests := []struct {
		name     string
		proves   string
		quantity string
		want     string
	}{
		{
			name:     "zero usage",
			proves:   "no usage reaches no tier, so no flat fee and no charge",
			quantity: "0",
			want:     "0",
		},
		{
			name:     "inside the first tier",
			proves:   "a small quantity is charged at the first rate",
			quantity: "500",
			want:     "5",
		},
		{
			name:     "exactly at the first boundary",
			proves:   "the bound is inclusive, so 1000 stays in tier one",
			quantity: "1000",
			want:     "10",
		},
		{
			name:     "one above the first boundary",
			proves:   "ALL 1001 units drop to the cheaper rate -- the bill goes DOWN as usage goes up, which is what volume pricing means",
			quantity: "1001",
			want:     "5.005",
		},
		{
			name:     "in the unbounded tier",
			proves:   "every unit is priced at the last tier's rate once the total gets there",
			quantity: "50000",
			want:     "50",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyVolume(tiers, mustQuantity(t, tc.quantity))
			if err != nil {
				t.Fatalf("applyVolume: %v\nthis case proves: %s", err, tc.proves)
			}
			if want := mustAmount(t, tc.want); got != want {
				t.Errorf("= %s, want %s\nthis case proves: %s", got, want, tc.proves)
			}
		})
	}
}

// The same quantity priced both ways must differ once a boundary is crossed.
// If these ever agreed, one of the two models would not be implemented.
func TestGraduatedAndVolumeDifferAcrossABoundary(t *testing.T) {
	tiers := []Tier{
		{UpTo: upTo(mustQuantity(t, "1000")), UnitPrice: mustAmount(t, "0.01")},
		{UpTo: nil, UnitPrice: mustAmount(t, "0.001")},
	}
	quantity := mustQuantity(t, "2000")

	graduated, err := applyGraduated(tiers, quantity)
	if err != nil {
		t.Fatalf("applyGraduated: %v", err)
	}
	volume, err := applyVolume(tiers, quantity)
	if err != nil {
		t.Fatalf("applyVolume: %v", err)
	}

	if graduated == volume {
		t.Fatalf("both models gave %s; they must differ once a boundary is crossed", graduated)
	}
	if want := mustAmount(t, "11"); graduated != want {
		t.Errorf("graduated = %s, want %s", graduated, want)
	}
	if want := mustAmount(t, "2"); volume != want {
		t.Errorf("volume = %s, want %s", volume, want)
	}
}

func TestFlatPrice(t *testing.T) {
	price := Price{Meter: "api_calls", Model: Flat, UnitPrice: mustAmount(t, "0.0025")}

	got, err := applyPrice(price, mustQuantity(t, "4000"))
	if err != nil {
		t.Fatalf("applyPrice: %v", err)
	}
	if want := mustAmount(t, "10"); got != want {
		t.Errorf("= %s, want %s", got, want)
	}
}

// INVARIANT I5. Rating the same input must produce a byte-identical result,
// however many times it runs and whatever order the input arrives in.
func TestRatingIsDeterministic(t *testing.T) {
	registry := testRegistry(t)
	plan := Plan{
		Name: "standard",
		Prices: []Price{
			{Meter: "api_calls", Model: Flat, UnitPrice: mustAmount(t, "0.001")},
			{Meter: "gb_egress", Model: Flat, UnitPrice: mustAmount(t, "0.09")},
		},
	}

	usages := []Usage{
		{Meter: "api_calls", Quantity: mustQuantity(t, "1500")},
		{Meter: "gb_egress", Quantity: mustQuantity(t, "12.5")},
		{Meter: "api_calls", Quantity: mustQuantity(t, "300")},
	}

	first, err := Rate(usages, plan, registry)
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}

	for i := 0; i < 200; i++ {
		again, err := Rate(usages, plan, registry)
		if err != nil {
			t.Fatalf("Rate %d: %v", i, err)
		}
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs from the first:\n%+v\n%+v", i, first, again)
		}
	}

	// Shuffling the input must not change the output. This is what would break
	// if line items came out of a map.
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 50; i++ {
		shuffled := append([]Usage(nil), usages...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })

		got, err := Rate(shuffled, plan, registry)
		if err != nil {
			t.Fatalf("Rate shuffled: %v", err)
		}
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("input order changed the output:\n%+v\n%+v", first, got)
		}
	}
}

// Usage for one meter is summed before pricing, not priced per event and added
// up. With tiers those give different answers -- pricing a thousand events
// individually puts every one in the first tier and never reaches a discount.
func TestUsageIsAggregatedBeforePricing(t *testing.T) {
	registry := testRegistry(t)
	plan := Plan{
		Name: "tiered",
		Prices: []Price{{
			Meter: "api_calls",
			Model: Graduated,
			Tiers: []Tier{
				{UpTo: upTo(mustQuantity(t, "1000")), UnitPrice: mustAmount(t, "0.01")},
				{UpTo: nil, UnitPrice: mustAmount(t, "0.001")},
			},
		}},
	}

	// 1500 units arriving as 15 events of 100.
	usages := make([]Usage, 15)
	for i := range usages {
		usages[i] = Usage{Meter: "api_calls", Quantity: mustQuantity(t, "100")}
	}

	items, err := Rate(usages, plan, registry)
	if err != nil {
		t.Fatalf("Rate: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d line items, want 1 -- usage for a meter must collapse into one", len(items))
	}

	// 1000 at 0.01 plus 500 at 0.001 = 10.50. Priced per event it would be
	// 1500 x 0.01 = 15.00, never reaching the second tier.
	if want := mustAmount(t, "10.50"); items[0].Amount != want {
		t.Errorf("amount = %s, want %s -- events were priced separately instead of aggregated", items[0].Amount, want)
	}
	if want := mustQuantity(t, "1500"); items[0].Quantity != want {
		t.Errorf("quantity = %s, want %s", items[0].Quantity, want)
	}
}

// An unregistered meter is an error, never a silent zero. Closes D12: a client
// sending "api_call" instead of "api_calls" would otherwise be billed nothing
// and nobody would find out.
func TestUnknownMeterIsRefused(t *testing.T) {
	registry := testRegistry(t)
	plan := Plan{Name: "standard", Prices: []Price{
		{Meter: "api_calls", Model: Flat, UnitPrice: mustAmount(t, "0.01")},
	}}

	_, err := Rate([]Usage{{Meter: "api_call", Quantity: mustQuantity(t, "100")}}, plan, registry)
	if !errors.Is(err, ErrUnknownMeter) {
		t.Fatalf("error = %v, want ErrUnknownMeter -- a typo'd meter must not bill zero silently", err)
	}
}

// A registered meter with no price in the plan is also an error. Usage that
// nobody has priced is revenue quietly going missing.
func TestMeterWithoutAPriceIsRefused(t *testing.T) {
	registry := testRegistry(t)
	plan := Plan{Name: "partial", Prices: []Price{
		{Meter: "api_calls", Model: Flat, UnitPrice: mustAmount(t, "0.01")},
	}}

	_, err := Rate([]Usage{{Meter: "gb_egress", Quantity: mustQuantity(t, "5")}}, plan, registry)
	if !errors.Is(err, ErrNoPrice) {
		t.Fatalf("error = %v, want ErrNoPrice", err)
	}
}

func TestTierValidation(t *testing.T) {
	tests := []struct {
		name   string
		proves string
		tiers  []Tier
	}{
		{
			name:   "no tiers",
			proves: "a tiered price with no tiers would charge nothing for everything",
			tiers:  nil,
		},
		{
			name:   "last tier is bounded",
			proves: "without a final unbounded tier, usage above the last bound has NO price and is silently free",
			tiers: []Tier{
				{UpTo: upTo(1000), UnitPrice: 100},
				{UpTo: upTo(2000), UnitPrice: 50},
			},
		},
		{
			name:   "unbounded tier is not last",
			proves: "an unbounded tier swallows everything after it, making later tiers unreachable",
			tiers: []Tier{
				{UpTo: nil, UnitPrice: 100},
				{UpTo: upTo(2000), UnitPrice: 50},
			},
		},
		{
			name:   "bounds do not ascend",
			proves: "a tier that does not exceed its predecessor is empty, and almost certainly a typo",
			tiers: []Tier{
				{UpTo: upTo(1000), UnitPrice: 100},
				{UpTo: upTo(500), UnitPrice: 50},
				{UpTo: nil, UnitPrice: 10},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateTiers(tc.tiers); !errors.Is(err, ErrInvalidPrice) {
				t.Errorf("error = %v, want ErrInvalidPrice\nthis case proves: %s", err, tc.proves)
			}
		})
	}
}

// Rounding happens once per meter, half-up, and is exercised where it actually
// bites: a price with more precision than the ledger stores.
func TestRoundingIsHalfUpAndHappensOnce(t *testing.T) {
	tests := []struct {
		name     string
		proves   string
		quantity string
		price    string
		want     string
	}{
		{
			name:     "exact",
			proves:   "a product that lands on the scale is untouched",
			quantity: "3", price: "0.01", want: "0.03",
		},
		{
			name:     "rounds up at the halfway point",
			proves:   "half-up is what people expect money to do",
			quantity: "0.0000005", price: "1", want: "0.000001",
		},
		{
			name:     "rounds down below halfway",
			proves:   "below the midpoint the extra precision is dropped",
			quantity: "0.0000004", price: "1", want: "0",
		},
		{
			name:     "sub-cent price over many units stays exact",
			proves:   "the reason the ledger has six places and not two",
			quantity: "1000000", price: "0.0001", want: "100",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := multiply(mustQuantity(t, tc.quantity), mustAmount(t, tc.price))
			if err != nil {
				t.Fatalf("multiply: %v", err)
			}
			if want := mustAmount(t, tc.want); got != want {
				t.Errorf("= %s, want %s\nthis case proves: %s", got, want, tc.proves)
			}
		})
	}
}

// Large quantities multiplied by real prices must not overflow silently. The
// product carries 15 decimal places before rounding, which is where int64
// arithmetic would have wrapped.
func TestLargeMultiplicationIsExact(t *testing.T) {
	got, err := multiply(mustQuantity(t, "9000000000"), mustAmount(t, "0.000001"))
	if err != nil {
		t.Fatalf("multiply: %v", err)
	}
	if want := mustAmount(t, "9000"); got != want {
		t.Errorf("= %s, want %s", got, want)
	}
}

// INVARIANT I6, enforced structurally rather than by review.
//
// No floating point may appear anywhere under billing/. Binary floating point
// cannot represent most decimal fractions, so a single float64 anywhere on the
// money path reintroduces the accumulation error every other decision in this
// project exists to avoid -- and it would do so silently.
//
// Checked by parsing each file rather than searching its bytes. A plain text
// search flags the comments that EXPLAIN why float is avoided, which are
// exactly the lines worth keeping -- the first version of this test failed on
// three of them. Comments are absent from the AST unless asked for, so parsing
// draws the line in the right place automatically.
//
// Test files are skipped: a test may legitimately use a float to construct an
// input that must be rejected.
func TestNoFloatingPointOnTheMoneyPath(t *testing.T) {
	forbidden := map[string]bool{"float64": true, "float32": true}

	// The test's working directory is its own package, so ".." is billing/.
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			ident, ok := n.(*ast.Ident)
			if ok && forbidden[ident.Name] {
				t.Errorf("%s uses %s at %s -- floating point must never touch money or quantity (I6)",
					filepath.ToSlash(path), ident.Name, fset.Position(ident.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking billing/: %v", err)
	}
}

func TestRegistryRejectsDuplicates(t *testing.T) {
	if _, err := NewRegistry(Meter{Name: "api_calls"}, Meter{Name: "api_calls"}); err == nil {
		t.Error("a duplicate meter was accepted")
	}
	if _, err := NewRegistry(Meter{Name: ""}); err == nil {
		t.Error("a nameless meter was accepted")
	}
}
