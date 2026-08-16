// Package posting turns metered usage into ledger entries.
//
// It is the wire between the two halves of the system: pricing knows how to
// turn quantities into money, the ledger knows how to record money, and until
// now nothing joined them. This package reads the usage a tenant accrued in a
// period, prices it, and posts one balanced transaction.
//
// See ADR-0014.
package posting

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/ledger"
	"github.com/RasheedHD/LedgerLine/billing/pricing"
)

// Period is the window of usage a posting run covers.
//
// Deliberately just a labelled time range. The open/closing/closed state
// machine that invariant I4 needs is Phase 6 work, and inventing half of it
// here would have to be undone.
type Period struct {
	// Label identifies the period and is part of the ledger transaction's
	// idempotency key, so it must be stable for a given window -- "2026-08",
	// not "august" one day and "2026-8" the next.
	Label string

	// Start is inclusive, End exclusive. Half-open so consecutive periods
	// neither overlap nor leave a gap: an event at exactly midnight belongs to
	// one period, not both and not neither.
	Start time.Time
	End   time.Time
}

// Poster reads usage, prices it, and posts it to the ledger.
type Poster struct {
	db       *sql.DB
	ledger   *ledger.Store
	registry *pricing.Registry
}

// Result describes what a posting run did.
type Result struct {
	// TransactionID is the ledger transaction, zero when nothing was posted.
	TransactionID int64

	// AlreadyPosted is true when this tenant and period had already been
	// posted and this run changed nothing.
	AlreadyPosted bool

	// LineItems is what the usage priced out to, in meter order.
	LineItems []pricing.LineItem

	// Total is the sum of the line items.
	Total ledger.Amount
}

var (
	// ErrNothingToPost means the tenant had no billable usage in the period.
	ErrNothingToPost = errors.New("posting: no billable usage in period")

	// ErrInvalidPeriod means the period's bounds do not make sense.
	ErrInvalidPeriod = errors.New("posting: invalid period")
)

// New returns a Poster.
func New(db *sql.DB, ledgerStore *ledger.Store, registry *pricing.Registry) *Poster {
	return &Poster{db: db, ledger: ledgerStore, registry: registry}
}

// usageForPeriod sums each meter's quantity for one tenant in one window.
//
// Aggregated in SQL rather than by reading every event into Go. Postgres sums
// NUMERIC exactly, so nothing is lost, and a busy tenant can have millions of
// events in a month that there is no reason to move across the wire.
//
// occurred_at, not received_at: which period usage belongs to is decided by
// when it happened, not when we heard about it. That is the whole point of
// ADR-0001's two clocks, and it is what makes a late event land in the period
// it actually belongs to.
const usageForPeriod = `
SELECT meter, SUM(quantity)::text
FROM events
WHERE tenant_id = $1 AND occurred_at >= $2 AND occurred_at < $3
GROUP BY meter
ORDER BY meter`

// Usage returns the tenant's aggregated usage for a period.
func (p *Poster) Usage(ctx context.Context, tenantID string, period Period) ([]pricing.Usage, error) {
	if !period.End.After(period.Start) {
		return nil, fmt.Errorf("%w: end %s is not after start %s", ErrInvalidPeriod, period.End, period.Start)
	}

	rows, err := p.db.QueryContext(ctx, usageForPeriod, tenantID, period.Start, period.End)
	if err != nil {
		return nil, fmt.Errorf("read usage: %w", err)
	}
	defer rows.Close()

	var usages []pricing.Usage
	for rows.Next() {
		var meter, total string
		if err := rows.Scan(&meter, &total); err != nil {
			return nil, fmt.Errorf("scan usage: %w", err)
		}

		quantity, err := pricing.ParseQuantity(total)
		if err != nil {
			// A sum that will not fit a Quantity is D33 showing up in
			// practice. Loud rather than truncated: billing the wrong amount
			// is worse than failing to bill.
			return nil, fmt.Errorf("meter %q total %q: %w", meter, total, err)
		}
		usages = append(usages, pricing.Usage{Meter: meter, Quantity: quantity})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage: %w", err)
	}
	return usages, nil
}

// Post prices a tenant's usage for a period and records it in the ledger.
//
// Idempotent: running it twice for the same tenant and period posts once. The
// second call reports AlreadyPosted and changes nothing.
func (p *Poster) Post(ctx context.Context, tenantID string, period Period, plan pricing.Plan) (Result, error) {
	var result Result

	usages, err := p.Usage(ctx, tenantID, period)
	if err != nil {
		return result, err
	}
	if len(usages) == 0 {
		return result, ErrNothingToPost
	}

	items, err := pricing.Rate(usages, plan, p.registry)
	if err != nil {
		return result, fmt.Errorf("rate usage for %q: %w", tenantID, err)
	}
	result.LineItems = items

	receivable, err := p.ledger.EnsureAccount(ctx, ReceivableAccount(tenantID), ledger.Asset)
	if err != nil {
		return result, err
	}

	transfers := make([]ledger.Transfer, 0, len(items))
	for _, item := range items {
		// A zero line item is skipped, not posted.
		//
		// ledger.Transfer requires a positive amount, and rightly: a zero
		// posting balances trivially while recording nothing, which hides
		// whatever produced it. Usage can legitimately price to zero -- a free
		// tier, a meter with a zero rate -- so this is expected, not an error.
		if item.Amount == 0 {
			continue
		}

		revenue, err := p.ledger.EnsureAccount(ctx, RevenueAccount(item.Meter), ledger.Revenue)
		if err != nil {
			return result, err
		}

		// Debit the tenant's receivable, credit the meter's revenue. This is
		// revenue recognition: the customer now owes us (an asset increases)
		// and we have earned it (revenue increases).
		transfers = append(transfers, ledger.Transfer{
			Debit:  receivable,
			Credit: revenue,
			Amount: item.Amount,
		})

		total, err := result.Total.Add(item.Amount)
		if err != nil {
			return result, fmt.Errorf("total for %q: %w", tenantID, err)
		}
		result.Total = total
	}

	if len(transfers) == 0 {
		// Usage existed but all of it priced to zero. Nothing to record, and
		// an empty transaction would be a lie about work having been done.
		return result, ErrNothingToPost
	}

	// CRITICAL: the idempotency key is derived, not generated.
	//
	// Same tenant and period always produce the same key, so a posting run
	// repeated after a crash, a retry, or an operator running it twice records
	// the usage once. A random key would post the same revenue again every
	// time, and the ledger would balance perfectly while being wrong.
	key := TransactionKey(tenantID, period.Label)

	// occurred_at is the period's start, not now: the transaction belongs to
	// the period whose usage it records, however long afterwards it is posted.
	tx, err := ledger.NewTransaction(key, period.Start,
		fmt.Sprintf("usage for %s in %s", tenantID, period.Label), transfers...)
	if err != nil {
		return result, fmt.Errorf("build transaction for %q: %w", tenantID, err)
	}

	id, already, err := p.ledger.Post(ctx, tx)
	if err != nil {
		return result, fmt.Errorf("post transaction for %q: %w", tenantID, err)
	}

	result.TransactionID = id
	result.AlreadyPosted = already
	return result, nil
}
