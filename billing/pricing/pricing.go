// Package pricing turns quantities of usage into amounts of money.
//
// CRITICAL PROPERTY: rating is a pure function. No clock, no database, no
// randomness, no dependence on map iteration order. The same input produces the
// same output forever.
//
// That is what invariant I5 -- deterministic replay -- rests on, and it is also
// what makes a disputed invoice reproducible six months later. Every design
// choice here that looks awkward, particularly plans holding a slice rather
// than a map, exists to protect it.
//
// See ADR-0011.
package pricing

import (
	"errors"
	"fmt"
	"math/big"
	"sort"

	"github.com/RasheedHD/LedgerLine/billing/ledger"
)

// Model selects how a price turns quantity into money.
type Model string

const (
	// Flat charges UnitPrice for every unit, however many there are.
	Flat Model = "flat"

	// Graduated splits the quantity across tiers, charging each portion at its
	// own tier's rate. The first 1000 units at one price, the next 9000 at
	// another, and so on. Stripe calls this "graduated".
	Graduated Model = "graduated"

	// Volume finds the single tier the TOTAL quantity lands in and charges
	// every unit at that tier's rate. Crossing a boundary makes the earlier
	// units cheaper too, which is the point -- it is a bulk discount rather
	// than a progressive scale.
	Volume Model = "volume"
)

// Tier is one band of a graduated or volume price.
type Tier struct {
	// UpTo is the inclusive upper bound of this tier. A nil UpTo means the tier
	// is unbounded and must be the last one.
	//
	// A pointer rather than a sentinel like -1 or MaxInt64, so "unbounded" is
	// unmistakable at the call site and cannot be confused with a real bound.
	UpTo *Quantity

	// UnitPrice is charged per unit falling in this tier.
	UnitPrice ledger.Amount

	// FlatFee is charged once if any quantity reaches this tier. Zero for most
	// prices.
	FlatFee ledger.Amount
}

// Price says how one meter is charged.
type Price struct {
	Meter string
	Model Model

	// UnitPrice is used by Flat and ignored otherwise.
	UnitPrice ledger.Amount

	// Tiers is used by Graduated and Volume, in ascending bound order.
	Tiers []Tier
}

// Plan is the set of prices a tenant is billed under.
type Plan struct {
	Name string

	// A slice, not a map keyed by meter.
	//
	// CRITICAL: Go randomises map iteration order deliberately. Rating that
	// walked a map would produce line items in a different order on every run,
	// and I5 asks for a byte-identical result. A slice has one order and keeps
	// it.
	Prices []Price
}

// Meter is a unit of usage that may be billed.
type Meter struct {
	Name string

	// Unit is for display only -- "call", "GB". It never affects arithmetic.
	Unit string
}

// Registry is the set of meters the system knows about.
//
// It exists so that a typo'd meter name is an error rather than a silent
// no-op. Without it, a client sending "api_call" instead of "api_calls" is
// billed nothing at all and nobody finds out, which is the undercounting
// failure mode invariant I3 is most worried about. Closes D12.
type Registry struct {
	meters map[string]Meter
}

// Usage is an aggregate quantity for one meter.
type Usage struct {
	Meter    string
	Quantity Quantity
}

// LineItem is the result of pricing one meter's usage.
type LineItem struct {
	Meter    string
	Quantity Quantity
	Amount   ledger.Amount
}

var (
	// ErrUnknownMeter means usage arrived for a meter not in the registry.
	ErrUnknownMeter = errors.New("pricing: unknown meter")

	// ErrNoPrice means the plan has no price for a meter that has usage.
	ErrNoPrice = errors.New("pricing: plan has no price for meter")

	// ErrInvalidPrice means a price is malformed and cannot be applied.
	ErrInvalidPrice = errors.New("pricing: invalid price")
)

// NewRegistry builds a registry, rejecting duplicates.
func NewRegistry(meters ...Meter) (*Registry, error) {
	r := &Registry{meters: make(map[string]Meter, len(meters))}
	for _, m := range meters {
		if m.Name == "" {
			return nil, errors.New("pricing: meter name is required")
		}
		if _, exists := r.meters[m.Name]; exists {
			return nil, fmt.Errorf("pricing: meter %q registered twice", m.Name)
		}
		r.meters[m.Name] = m
	}
	return r, nil
}

// Known reports whether a meter is registered.
func (r *Registry) Known(name string) bool {
	_, ok := r.meters[name]
	return ok
}

// Rate prices a set of usage against a plan.
//
// Usage for the same meter is summed before being priced, not priced
// separately and added up. That matters for tiered prices -- pricing a thousand
// separate events individually would put every one of them in the first tier
// and never reach a discount -- and it also confines rounding to one operation
// per meter instead of one per event.
//
// Output is sorted by meter name so the result is byte-identical across runs
// regardless of input order. I5 again.
func Rate(usages []Usage, plan Plan, registry *Registry) ([]LineItem, error) {
	totals := make(map[string]Quantity, len(usages))
	for _, u := range usages {
		if !registry.Known(u.Meter) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownMeter, u.Meter)
		}
		if u.Quantity < 0 {
			return nil, fmt.Errorf("pricing: meter %q has negative usage", u.Meter)
		}
		sum, err := totals[u.Meter].Add(u.Quantity)
		if err != nil {
			return nil, fmt.Errorf("total for meter %q: %w", u.Meter, err)
		}
		totals[u.Meter] = sum
	}

	// The map above is only ever an accumulator. Its keys are pulled out and
	// sorted before anything depends on their order.
	meters := make([]string, 0, len(totals))
	for meter := range totals {
		meters = append(meters, meter)
	}
	sort.Strings(meters)

	items := make([]LineItem, 0, len(meters))
	for _, meter := range meters {
		price, ok := priceFor(plan, meter)
		if !ok {
			return nil, fmt.Errorf("%w: %q under plan %q", ErrNoPrice, meter, plan.Name)
		}

		amount, err := applyPrice(price, totals[meter])
		if err != nil {
			return nil, fmt.Errorf("pricing meter %q: %w", meter, err)
		}

		items = append(items, LineItem{
			Meter:    meter,
			Quantity: totals[meter],
			Amount:   amount,
		})
	}
	return items, nil
}

// priceFor finds a meter's price. Linear over a slice rather than a map lookup,
// which is both fast enough for the handful of prices a plan holds and free of
// any iteration-order question.
func priceFor(plan Plan, meter string) (Price, bool) {
	for _, p := range plan.Prices {
		if p.Meter == meter {
			return p, true
		}
	}
	return Price{}, false
}

func applyPrice(price Price, quantity Quantity) (ledger.Amount, error) {
	switch price.Model {
	case Flat:
		return multiply(quantity, price.UnitPrice)
	case Graduated:
		return applyGraduated(price.Tiers, quantity)
	case Volume:
		return applyVolume(price.Tiers, quantity)
	default:
		return 0, fmt.Errorf("%w: unknown model %q", ErrInvalidPrice, price.Model)
	}
}

// applyGraduated charges each portion of the quantity at its own tier's rate.
func applyGraduated(tiers []Tier, quantity Quantity) (ledger.Amount, error) {
	if err := validateTiers(tiers); err != nil {
		return 0, err
	}

	var total ledger.Amount
	var consumed Quantity

	for _, tier := range tiers {
		if consumed >= quantity {
			break
		}

		// How much of the quantity falls inside this tier.
		var portion Quantity
		if tier.UpTo == nil {
			portion = quantity - consumed
		} else {
			upper := *tier.UpTo
			if upper > quantity {
				upper = quantity
			}
			portion = upper - consumed
		}
		if portion <= 0 {
			continue
		}

		amount, err := multiply(portion, tier.UnitPrice)
		if err != nil {
			return 0, err
		}
		next, err := total.Add(amount)
		if err != nil {
			return 0, err
		}
		next, err = next.Add(tier.FlatFee)
		if err != nil {
			return 0, err
		}
		total = next
		consumed += portion
	}

	return total, nil
}

// applyVolume charges every unit at the rate of the tier the total lands in.
func applyVolume(tiers []Tier, quantity Quantity) (ledger.Amount, error) {
	if err := validateTiers(tiers); err != nil {
		return 0, err
	}
	if quantity == 0 {
		// No usage means no tier is reached, so no flat fee either. Charging
		// one here would bill a customer who did nothing.
		return 0, nil
	}

	for _, tier := range tiers {
		if tier.UpTo == nil || quantity <= *tier.UpTo {
			amount, err := multiply(quantity, tier.UnitPrice)
			if err != nil {
				return 0, err
			}
			return amount.Add(tier.FlatFee)
		}
	}

	// validateTiers guarantees a final unbounded tier, so this is unreachable.
	return 0, fmt.Errorf("%w: quantity %s exceeded every tier", ErrInvalidPrice, quantity)
}

func validateTiers(tiers []Tier) error {
	if len(tiers) == 0 {
		return fmt.Errorf("%w: no tiers", ErrInvalidPrice)
	}

	var previous Quantity
	for i, tier := range tiers {
		last := i == len(tiers)-1

		if tier.UpTo == nil {
			if !last {
				return fmt.Errorf("%w: unbounded tier %d is not last", ErrInvalidPrice, i)
			}
			continue
		}
		if last {
			// Without a final unbounded tier, a quantity above the last bound
			// has no price at all -- and silently charging nothing for the
			// excess is the failure that never gets reported.
			return fmt.Errorf("%w: last tier is bounded, so usage above %s has no price", ErrInvalidPrice, *tier.UpTo)
		}
		if *tier.UpTo <= previous {
			return fmt.Errorf("%w: tier %d bound %s does not exceed the previous %s", ErrInvalidPrice, i, *tier.UpTo, previous)
		}
		previous = *tier.UpTo
	}
	return nil
}

// multiply computes quantity x unitPrice exactly, then rounds to the ledger's
// scale.
//
// CRITICAL: this is the only place the two scales meet, and it is done in
// big.Int rather than int64.
//
// A quantity carries 9 decimal places and a price carries 6, so their product
// carries 15. Even modest values overflow int64 at that width -- 1000 units at
// $0.01 is 10^12 x 10^4, which is fine, but a hundred thousand units at a
// realistic price is not. big.Int makes the multiplication exact regardless of
// magnitude, and the result is checked back into int64 afterwards.
//
// Rounding is half-up on the absolute value. Half-up is what people expect
// money to do, and it is applied ONCE per meter rather than per event, because
// Rate sums quantities before pricing them.
func multiply(quantity Quantity, unitPrice ledger.Amount) (ledger.Amount, error) {
	if quantity == 0 || unitPrice == 0 {
		return 0, nil
	}

	product := new(big.Int).Mul(
		big.NewInt(int64(quantity)),
		big.NewInt(int64(unitPrice)),
	)

	// The product is scaled by 10^(9+6); the result wants 10^6, so divide by
	// 10^9.
	divisor := big.NewInt(quantityFactor)

	quotient, remainder := new(big.Int).QuoRem(product, divisor, new(big.Int))

	// Half-up: if twice the remainder reaches the divisor, round away from
	// zero.
	doubled := new(big.Int).Abs(remainder)
	doubled.Lsh(doubled, 1)
	if doubled.Cmp(divisor) >= 0 {
		if product.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}

	if !quotient.IsInt64() {
		return 0, fmt.Errorf("%w: %s x %s does not fit in an amount",
			ledger.ErrOverflow, quantity, unitPrice)
	}
	return ledger.Amount(quotient.Int64()), nil
}
