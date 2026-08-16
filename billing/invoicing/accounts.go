package invoicing

import "fmt"

// The chart of accounts is generated from names rather than configured.
//
// A usage-billing ledger needs an account per tenant on the asset side -- you
// cannot answer "what does this customer owe" from a single pooled receivable
// -- and an account per meter on the revenue side, so revenue can be reported
// by product line rather than as one undifferentiated number.
//
// Neither set can be known in advance: tenants and meters arrive over time. So
// the names are derived deterministically and the accounts are created on
// demand via ledger.EnsureAccountTx.
//
// The prefixes matter. Without them a tenant called "api_calls" would collide
// with the revenue account for the api_calls meter, and two entirely unrelated
// figures would accumulate in one place. Prefixing makes the namespaces
// disjoint by construction.
const (
	receivablePrefix = "receivable:"
	revenuePrefix    = "revenue:"
)

// ReceivableAccount names the asset account holding what a tenant owes.
func ReceivableAccount(tenantID string) string {
	return receivablePrefix + tenantID
}

// RevenueAccount names the revenue account for a meter.
func RevenueAccount(meter string) string {
	return revenuePrefix + meter
}

// TransactionKey is the ledger idempotency key for a tenant's billing run in a
// period.
//
// Derived rather than random, so the same run repeated -- after a crash, a
// retry, or an operator running it twice -- records the revenue once. A random
// key would post the same revenue again on every run and the ledger would
// balance perfectly while being wrong, which is the one failure double-entry
// cannot catch: both sides of a duplicate entry are equally wrong.
func TransactionKey(tenantID, periodLabel string) string {
	return fmt.Sprintf("usage:%s:%s", tenantID, periodLabel)
}
