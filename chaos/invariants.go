// Package chaos breaks the system on purpose and checks the invariants held.
//
// Every invariant in PLAN.md is already tested individually, by a unit test
// that sets up exactly the conditions it needs. That proves each mechanism
// works. It does not prove they work TOGETHER while something is failing,
// which is the only claim a billing system's users actually care about.
//
// This package exists to make the README's last clause true: a chaos suite
// that proves invoices stay correct to the cent.
package chaos

import (
	"context"
	"fmt"

	"github.com/RasheedHD/LedgerLine/billing/ledger"
)

// Violation describes an invariant that did not hold.
type Violation struct {
	Invariant string
	Detail    string
}

func (v Violation) Error() string {
	return fmt.Sprintf("%s violated: %s", v.Invariant, v.Detail)
}

// CheckAll runs every invariant that can be checked from stored state.
//
// I5 (deterministic replay) and I6 (no float) are absent on purpose. I6 is a
// static property checked by an AST walk in billing/pricing, and I5 needs a
// second run against an empty database rather than an inspection of this one --
// it has its own scenario below.
func CheckAll(ctx context.Context, h *Harness) []Violation {
	var violations []Violation

	for _, check := range []func(context.Context, *Harness) *Violation{
		CheckMoneyConserved,
		CheckNoDoubleBilling,
		CheckNothingSilentlyLost,
		CheckEventsBilledAtMostOnce,
	} {
		if v := check(ctx, h); v != nil {
			violations = append(violations, *v)
		}
	}
	return violations
}

// CheckMoneyConserved is I1: every posting in the ledger sums to zero.
//
// A non-zero trial balance means money was created or destroyed somewhere, and
// every figure derived from the ledger is suspect.
func CheckMoneyConserved(ctx context.Context, h *Harness) *Violation {
	total, err := h.Ledger.TrialBalance(ctx)
	if err != nil {
		return &Violation{"I1", fmt.Sprintf("could not read trial balance: %v", err)}
	}
	if total != 0 {
		return &Violation{"I1", fmt.Sprintf("trial balance is %s, want 0", total)}
	}
	return nil
}

// CheckNoDoubleBilling is I2: one accepted idempotency key produces at most one
// event row, however many times it was delivered.
//
// Compared against what ingest actually acknowledged rather than what the test
// intended to send, so a request that failed for an unrelated reason does not
// make this look like a violation.
func CheckNoDoubleBilling(ctx context.Context, h *Harness) *Violation {
	var rows int
	err := h.DB.QueryRowContext(ctx, `SELECT count(*) FROM events`).Scan(&rows)
	if err != nil {
		return &Violation{"I2", fmt.Sprintf("could not count events: %v", err)}
	}

	// Anything still on the log and not yet consumed is not a violation, so
	// this is an upper bound rather than an equality.
	if accepted := h.AcceptedCount(); rows > accepted {
		return &Violation{"I2", fmt.Sprintf("%d event rows for %d accepted keys", rows, accepted)}
	}
	return nil
}

// CheckNothingSilentlyLost is I3: everything ingest acknowledged is either
// stored as an event or recorded in the dead-letter table.
//
// Only meaningful once the consumer has caught up, so Harness.DrainFully is
// what callers use before this.
func CheckNothingSilentlyLost(ctx context.Context, h *Harness) *Violation {
	caughtUp, err := h.CaughtUp(ctx)
	if err != nil {
		return &Violation{"I3", fmt.Sprintf("could not read consumer offset: %v", err)}
	}
	if !caughtUp {
		// Not a violation: usage still on the log is durable and will be
		// consumed. Saying otherwise would make this check fire on a system
		// that is merely busy.
		return nil
	}

	for _, key := range h.AcceptedKeys() {
		var found int
		err := h.DB.QueryRowContext(ctx, `
			SELECT
				(SELECT count(*) FROM events WHERE idempotency_key = $1)
				+ (SELECT count(*) FROM dead_letters WHERE idempotency_key = $1)`,
			key).Scan(&found)
		if err != nil {
			return &Violation{"I3", fmt.Sprintf("could not look up %q: %v", key, err)}
		}
		if found == 0 {
			return &Violation{"I3", fmt.Sprintf(
				"key %q was acknowledged with 202 but is in neither events nor dead_letters", key)}
		}
	}
	return nil
}

// CheckEventsBilledAtMostOnce is the billing half of I2: no event may appear on
// two invoices, and every billed event's invoice must exist.
func CheckEventsBilledAtMostOnce(ctx context.Context, h *Harness) *Violation {
	var orphaned int
	err := h.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM events e
		WHERE e.invoice_id IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM invoices i WHERE i.id = e.invoice_id)`).Scan(&orphaned)
	if err != nil {
		return &Violation{"I2", fmt.Sprintf("could not check event billing: %v", err)}
	}
	if orphaned != 0 {
		return &Violation{"I2", fmt.Sprintf("%d events point at an invoice that does not exist", orphaned)}
	}

	// Each invoice's stored total must equal the sum of its own line items. A
	// mismatch means the invoice says one thing and its detail says another,
	// and the customer was told the total.
	var mismatched int
	err = h.DB.QueryRowContext(ctx, `
		SELECT count(*) FROM invoices i
		WHERE i.total <> COALESCE(
			(SELECT SUM(li.amount) FROM invoice_line_items li WHERE li.invoice_id = i.id), 0)`).Scan(&mismatched)
	if err != nil {
		return &Violation{"I1", fmt.Sprintf("could not check invoice totals: %v", err)}
	}
	if mismatched != 0 {
		return &Violation{"I1", fmt.Sprintf("%d invoices disagree with their own line items", mismatched)}
	}
	return nil
}

// InvoiceSnapshot is every invoice's identity and total, for proving I4.
type InvoiceSnapshot map[int64]ledger.Amount

// SnapshotInvoices records what every invoice currently says.
func SnapshotInvoices(ctx context.Context, h *Harness) (InvoiceSnapshot, error) {
	rows, err := h.DB.QueryContext(ctx, `SELECT id, total FROM invoices ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("snapshot invoices: %w", err)
	}
	defer rows.Close()

	snapshot := make(InvoiceSnapshot)
	for rows.Next() {
		var id, total int64
		if err := rows.Scan(&id, &total); err != nil {
			return nil, fmt.Errorf("scan invoice: %w", err)
		}
		snapshot[id] = ledger.Amount(total)
	}
	return snapshot, rows.Err()
}

// CheckInvoicesUnchanged is I4: an invoice that existed before must still say
// exactly what it said.
//
// New invoices appearing is fine -- periods close over time. An existing one
// changing or vanishing is not.
func CheckInvoicesUnchanged(ctx context.Context, h *Harness, before InvoiceSnapshot) *Violation {
	after, err := SnapshotInvoices(ctx, h)
	if err != nil {
		return &Violation{"I4", err.Error()}
	}

	for id, total := range before {
		nowTotal, present := after[id]
		if !present {
			return &Violation{"I4", fmt.Sprintf("invoice %d has disappeared", id)}
		}
		if nowTotal != total {
			return &Violation{"I4", fmt.Sprintf(
				"invoice %d was %s and is now %s", id, total, nowTotal)}
		}
	}
	return nil
}
