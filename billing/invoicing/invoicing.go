// Package invoicing owns billing periods and the invoices they produce.
//
// Closing a period is the moment usage becomes money owed: unbilled events are
// gathered, priced, written as an invoice, marked as billed, and recognised in
// the ledger. All of it in one transaction, because an invoice with no ledger
// entry behind it -- or the reverse -- is worse than neither.
//
// See ADR-0015.
package invoicing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/ledger"
	"github.com/RasheedHD/LedgerLine/billing/pricing"
)

// State is where a period is in its lifecycle.
type State string

const (
	// Open accepts new usage and can be closed.
	Open State = "open"

	// Closed has an invoice. Nothing further is ever billed to it.
	Closed State = "closed"
)

// Period is a tenant's billing window.
type Period struct {
	ID       int64
	TenantID string
	Label    string

	// Half-open [Start, End). Consecutive periods then neither overlap nor
	// leave a gap.
	Start time.Time
	End   time.Time

	State    State
	ClosedAt time.Time
}

// Invoice is what a closed period produced.
type Invoice struct {
	ID                  int64
	PeriodID            int64
	TenantID            string
	Total               ledger.Amount
	LedgerTransactionID int64
	IssuedAt            time.Time
	LineItems           []LineItem
}

// LineItem is one meter's contribution to an invoice.
type LineItem struct {
	Meter    string
	Quantity pricing.Quantity
	Amount   ledger.Amount

	// Late is true when this line includes usage that occurred before the
	// period it is billed in -- ADR-0001 §5's roll-forward, made visible.
	Late bool
}

var (
	// ErrPeriodClosed means the period has already been invoiced.
	ErrPeriodClosed = errors.New("invoicing: period is already closed")

	// ErrNothingToBill means the period had no unbilled usage.
	ErrNothingToBill = errors.New("invoicing: no unbilled usage in period")

	// ErrPeriodNotFound means no such period exists.
	ErrPeriodNotFound = errors.New("invoicing: period not found")

	// ErrInvalidPeriod means the period's bounds do not make sense.
	ErrInvalidPeriod = errors.New("invoicing: invalid period")
)

// Service manages periods and closes them into invoices.
type Service struct {
	db       *sql.DB
	ledger   *ledger.Store
	registry *pricing.Registry
	now      func() time.Time
}

// New returns a Service.
func New(db *sql.DB, ledgerStore *ledger.Store, registry *pricing.Registry) *Service {
	return &Service{db: db, ledger: ledgerStore, registry: registry, now: time.Now}
}

const (
	ensurePeriod = `
INSERT INTO billing_periods (tenant_id, label, starts_at, ends_at, state, created_at)
VALUES ($1, $2, $3, $4, 'open', $5)
ON CONFLICT (tenant_id, label) DO UPDATE SET tenant_id = billing_periods.tenant_id
RETURNING id, tenant_id, label, starts_at, ends_at, state, COALESCE(closed_at, 'epoch'::timestamptz)`

	// FOR UPDATE takes a row lock held until the transaction ends.
	//
	// CRITICAL: this is what serialises two billing runs for the same period.
	// Without it both read state='open', both gather the same unbilled events,
	// and one loses on a unique constraint somewhere downstream -- correct by
	// accident rather than by design, which was D40.
	lockPeriod = `
SELECT id, tenant_id, label, starts_at, ends_at, state, COALESCE(closed_at, 'epoch'::timestamptz)
FROM billing_periods WHERE id = $1 FOR UPDATE`

	// Everything unbilled that happened before this period ends.
	//
	// CRITICAL: the lower bound is deliberately absent.
	//
	// Selecting only [start, end) would leave an event that arrived after its
	// own period closed unbilled forever -- it belongs to a window nobody will
	// gather again. Gathering everything still unbilled up to the end of this
	// period means such an event is picked up by the next run, which is
	// exactly ADR-0001 §5's roll-forward. The policy falls out of the model
	// rather than needing a special case.
	unbilledUsage = `
SELECT meter, SUM(quantity)::text, MIN(occurred_at)
FROM events
WHERE tenant_id = $1 AND invoice_id IS NULL AND occurred_at < $2
GROUP BY meter
ORDER BY meter`

	markBilled = `
UPDATE events SET invoice_id = $1
WHERE tenant_id = $2 AND invoice_id IS NULL AND occurred_at < $3`

	insertInvoice = `
INSERT INTO invoices (period_id, tenant_id, total, ledger_transaction_id, issued_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id`

	insertLineItem = `
INSERT INTO invoice_line_items (invoice_id, meter, quantity, amount)
VALUES ($1, $2, $3::numeric, $4)`

	closePeriod = `UPDATE billing_periods SET state = 'closed', closed_at = $2 WHERE id = $1`

	selectInvoiceByPeriod = `
SELECT id, period_id, tenant_id, total, ledger_transaction_id, issued_at
FROM invoices WHERE period_id = $1`

	selectLineItems = `
SELECT meter, quantity::text, amount FROM invoice_line_items
WHERE invoice_id = $1 ORDER BY meter`
)

// EnsurePeriod creates a period or returns the existing one with that label.
func (s *Service) EnsurePeriod(ctx context.Context, tenantID, label string, start, end time.Time) (Period, error) {
	if !end.After(start) {
		return Period{}, fmt.Errorf("%w: end %s is not after start %s", ErrInvalidPeriod, end, start)
	}
	if tenantID == "" || label == "" {
		return Period{}, fmt.Errorf("%w: tenant and label are required", ErrInvalidPeriod)
	}

	row := s.db.QueryRowContext(ctx, ensurePeriod, tenantID, label, start, end, s.now().UTC())
	return scanPeriod(row)
}

// Close bills a period: it gathers unbilled usage, prices it, issues an
// invoice, marks the events, and posts the revenue to the ledger.
//
// Idempotent. Closing an already-closed period returns its existing invoice
// rather than issuing a second one.
func (s *Service) Close(ctx context.Context, periodID int64, plan pricing.Plan) (Invoice, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Invoice{}, fmt.Errorf("begin: %w", err)
	}
	// A no-op after a successful commit, so it is safe unconditionally.
	defer tx.Rollback()

	period, err := scanPeriod(tx.QueryRowContext(ctx, lockPeriod, periodID))
	if err != nil {
		return Invoice{}, err
	}

	if period.State == Closed {
		// Already billed. Return what was issued rather than issuing again --
		// this is what makes a retried billing job safe.
		invoice, err := s.invoiceForPeriod(ctx, tx, period.ID)
		if err != nil {
			return Invoice{}, err
		}
		if err := tx.Commit(); err != nil {
			return Invoice{}, fmt.Errorf("commit: %w", err)
		}
		return invoice, ErrPeriodClosed
	}

	usages, lateBefore, err := s.gatherUsage(ctx, tx, period)
	if err != nil {
		return Invoice{}, err
	}
	if len(usages) == 0 {
		return Invoice{}, ErrNothingToBill
	}

	items, err := pricing.Rate(usages, plan, s.registry)
	if err != nil {
		return Invoice{}, fmt.Errorf("rate usage for %q: %w", period.TenantID, err)
	}

	transfers, total, lineItems, err := s.buildEntry(ctx, tx, period, items, lateBefore)
	if err != nil {
		return Invoice{}, err
	}
	if len(transfers) == 0 {
		// Usage existed but priced to zero throughout. Nothing to record, and
		// an empty invoice would claim work was billed that was not.
		return Invoice{}, ErrNothingToBill
	}

	ledgerTx, err := ledger.NewTransaction(
		TransactionKey(period.TenantID, period.Label),
		period.Start,
		fmt.Sprintf("usage for %s in %s", period.TenantID, period.Label),
		transfers...)
	if err != nil {
		return Invoice{}, fmt.Errorf("build ledger transaction: %w", err)
	}

	ledgerID, _, err := s.ledger.PostTx(ctx, tx, ledgerTx)
	if err != nil {
		return Invoice{}, fmt.Errorf("post to ledger: %w", err)
	}

	issuedAt := s.now().UTC()
	var invoiceID int64
	err = tx.QueryRowContext(ctx, insertInvoice,
		period.ID, period.TenantID, int64(total), ledgerID, issuedAt).Scan(&invoiceID)
	if err != nil {
		return Invoice{}, fmt.Errorf("insert invoice: %w", err)
	}

	for _, item := range lineItems {
		if _, err := tx.ExecContext(ctx, insertLineItem,
			invoiceID, item.Meter, item.Quantity.String(), int64(item.Amount)); err != nil {
			return Invoice{}, fmt.Errorf("insert line item %q: %w", item.Meter, err)
		}
	}

	// CRITICAL: marking the events must use the same predicate that gathered
	// them, in the same transaction.
	//
	// Anything else and an event that arrived between the SELECT and the UPDATE
	// would either be billed without being marked -- and so billed again next
	// period -- or marked without being billed, which loses it silently. Inside
	// one transaction the snapshot is stable, so the two sets are identical.
	if _, err := tx.ExecContext(ctx, markBilled, invoiceID, period.TenantID, period.End); err != nil {
		return Invoice{}, fmt.Errorf("mark events billed: %w", err)
	}

	if _, err := tx.ExecContext(ctx, closePeriod, period.ID, issuedAt); err != nil {
		return Invoice{}, fmt.Errorf("close period: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Invoice{}, fmt.Errorf("commit close: %w", err)
	}

	return Invoice{
		ID:                  invoiceID,
		PeriodID:            period.ID,
		TenantID:            period.TenantID,
		Total:               total,
		LedgerTransactionID: ledgerID,
		IssuedAt:            issuedAt,
		LineItems:           lineItems,
	}, nil
}

// gatherUsage returns unbilled usage per meter, and the earliest occurrence of
// each so lateness can be reported.
func (s *Service) gatherUsage(ctx context.Context, tx *sql.Tx, period Period) ([]pricing.Usage, map[string]time.Time, error) {
	rows, err := tx.QueryContext(ctx, unbilledUsage, period.TenantID, period.End)
	if err != nil {
		return nil, nil, fmt.Errorf("read unbilled usage: %w", err)
	}
	defer rows.Close()

	var usages []pricing.Usage
	earliest := make(map[string]time.Time)

	for rows.Next() {
		var meter, total string
		var first time.Time
		if err := rows.Scan(&meter, &total, &first); err != nil {
			return nil, nil, fmt.Errorf("scan usage: %w", err)
		}

		quantity, err := pricing.ParseQuantity(total)
		if err != nil {
			// A sum too large for a Quantity is D33 in practice. Loud rather
			// than truncated: billing the wrong amount is worse than failing.
			return nil, nil, fmt.Errorf("meter %q total %q: %w", meter, total, err)
		}
		usages = append(usages, pricing.Usage{Meter: meter, Quantity: quantity})
		earliest[meter] = first
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate usage: %w", err)
	}
	return usages, earliest, nil
}

// buildEntry turns priced line items into ledger transfers, skipping zeroes.
func (s *Service) buildEntry(
	ctx context.Context,
	tx *sql.Tx,
	period Period,
	items []pricing.LineItem,
	earliest map[string]time.Time,
) ([]ledger.Transfer, ledger.Amount, []LineItem, error) {
	receivable, err := s.ledger.EnsureAccountTx(ctx, tx, ReceivableAccount(period.TenantID), ledger.Asset)
	if err != nil {
		return nil, 0, nil, err
	}

	var transfers []ledger.Transfer
	var total ledger.Amount
	var lines []LineItem

	for _, item := range items {
		// Usage that prices to zero -- a free tier, a zero-rate meter -- is
		// recorded as a line so the invoice shows the usage happened, but
		// produces no transfer. ledger.Transfer requires a positive amount,
		// and rightly: a zero posting balances trivially while recording
		// nothing.
		line := LineItem{
			Meter:    item.Meter,
			Quantity: item.Quantity,
			Late:     earliest[item.Meter].Before(period.Start),
		}
		line.Amount = item.Amount
		lines = append(lines, line)

		if item.Amount == 0 {
			continue
		}

		revenue, err := s.ledger.EnsureAccountTx(ctx, tx, RevenueAccount(item.Meter), ledger.Revenue)
		if err != nil {
			return nil, 0, nil, err
		}

		// Debit the tenant's receivable, credit the meter's revenue: the
		// customer now owes us, and we have earned it.
		transfers = append(transfers, ledger.Transfer{
			Debit:  receivable,
			Credit: revenue,
			Amount: item.Amount,
		})

		next, err := total.Add(item.Amount)
		if err != nil {
			return nil, 0, nil, fmt.Errorf("invoice total for %q: %w", period.TenantID, err)
		}
		total = next
	}
	return transfers, total, lines, nil
}

// InvoiceForPeriod returns the invoice a period produced.
func (s *Service) InvoiceForPeriod(ctx context.Context, periodID int64) (Invoice, error) {
	return s.invoiceForPeriod(ctx, s.db, periodID)
}

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (s *Service) invoiceForPeriod(ctx context.Context, q queryer, periodID int64) (Invoice, error) {
	var inv Invoice
	var total int64

	err := q.QueryRowContext(ctx, selectInvoiceByPeriod, periodID).
		Scan(&inv.ID, &inv.PeriodID, &inv.TenantID, &total, &inv.LedgerTransactionID, &inv.IssuedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Invoice{}, fmt.Errorf("%w: period %d has no invoice", ErrPeriodNotFound, periodID)
	}
	if err != nil {
		return Invoice{}, fmt.Errorf("read invoice: %w", err)
	}
	inv.Total = ledger.Amount(total)

	rows, err := q.QueryContext(ctx, selectLineItems, inv.ID)
	if err != nil {
		return Invoice{}, fmt.Errorf("read line items: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var item LineItem
		var quantity string
		var amount int64
		if err := rows.Scan(&item.Meter, &quantity, &amount); err != nil {
			return Invoice{}, fmt.Errorf("scan line item: %w", err)
		}
		q, err := pricing.ParseQuantity(quantity)
		if err != nil {
			return Invoice{}, fmt.Errorf("line item quantity %q: %w", quantity, err)
		}
		item.Quantity = q
		item.Amount = ledger.Amount(amount)
		inv.LineItems = append(inv.LineItems, item)
	}
	if err := rows.Err(); err != nil {
		return Invoice{}, fmt.Errorf("iterate line items: %w", err)
	}
	return inv, nil
}

func scanPeriod(row *sql.Row) (Period, error) {
	var p Period
	var state string

	err := row.Scan(&p.ID, &p.TenantID, &p.Label, &p.Start, &p.End, &state, &p.ClosedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Period{}, ErrPeriodNotFound
	}
	if err != nil {
		return Period{}, fmt.Errorf("scan period: %w", err)
	}
	p.State = State(state)
	return p, nil
}
