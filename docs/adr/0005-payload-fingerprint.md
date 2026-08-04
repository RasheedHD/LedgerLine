# ADR-0005: Detecting a reused idempotency key

- **Status:** Accepted
- **Date:** 2026-08-03
- **Deciders:** Rasheed
- **Related:** [ADR-0002](0002-dedup-enforcement.md), [ADR-0004](0004-ingest-api-contract.md)
- **Resolves:** PLAN.md debt item D6

## Context

ADR-0002 identified this as the most likely correctness hole in the design:

> A client reusing a key for genuinely different usage would have its second
> event silently discarded and be told `202`. We would lose billable usage and
> report success.

The unique constraint only sees `(tenant_id, idempotency_key)`. It cannot tell
a legitimate retry from a client that recycled a key for a different event —
both look identical to it.

The failure direction matters. This undercharges the customer and returns a
success, so nobody complains and nobody notices. Overcharging generates a
support ticket within a billing cycle; undercharging generates silence and a
revenue gap that shows up, if ever, in an audit.

## Decision

**Store a SHA-256 fingerprint of each event's billable content. On a key
conflict, compare the stored fingerprint against the offered one, and reject a
mismatch with `409 Conflict` and code `idempotency_key_reuse`.**

### What is fingerprinted

`tenant_id`, `meter`, the canonicalised `quantity`, and `occurred_at` in UTC.

Two exclusions, both deliberate:

- **`received_at`** is ours and differs on every attempt. Including it would
  make every retry look like a different payload, breaking the endpoint
  entirely.
- **`idempotency_key`** is the lookup key, not part of the content being
  compared against it.

### Canonicalisation

A client that sends `"1"` and then `"1.0"` on its retry has sent the same
event. Fingerprinting the raw string would call that reuse and reject a
perfectly correct retry — turning a safety feature into an outage.

`quantity` is therefore normalised by stripping leading zeros from the integer
part and trailing zeros from the fraction: `"0001.500"` and `"1.5"` hash
identically. This is pure string manipulation. Parsing into a numeric type to
normalise would mean choosing a representation, and the obvious one is exactly
what ADR-0001 §3 forbids.

`occurred_at` is normalised to UTC, so the same instant written with different
offsets produces one fingerprint.

### Field encoding

**Each field is length-prefixed with a big-endian `uint64` before being
hashed.** This is the load-bearing detail and it is easy to miss.

Plain concatenation is ambiguous: tenant `"ab"` with meter `"c"` and tenant
`"a"` with meter `"bc"` both produce `"abc"`. Two genuinely different events
would share a fingerprint, and a reused key carrying one of them would be
accepted as a duplicate — reintroducing the exact bug this ADR exists to close.
A delimiter only moves the problem, since any byte chosen can also occur inside
a field. Length-prefixing is unambiguous regardless of content.

There is a test that fails if the prefix is removed.

### Why SHA-256

This is a correctness check where a collision means silently accepting
divergent usage as a duplicate. A cheaper non-cryptographic hash trades that
guarantee for a saving that is invisible next to the database round trip on the
same code path.

### Why `409` here but `202` for a plain duplicate

ADR-0004 argues that a duplicate deserves `202` because the client did exactly
the right thing after a dropped connection. Reuse is the opposite: the client
has a real bug, and the only useful response is to say so. `409 Conflict` is
accurate — the request conflicts with existing state — and it is not something
a retry can fix, which is precisely the signal the client needs.

### Nullable column

`payload_fingerprint` is nullable, and `NULL` means "this row predates
migration 000002, reuse cannot be detected for it."

A backfilled placeholder would be actively worse: two rows sharing a sentinel
would compare equal and claim their payloads matched. There is no honest way to
compute a fingerprint for an existing row, because the hash is defined over an
application-side canonicalisation that SQL cannot reproduce. The code checks
for `NULL` and declines to guess.

## Consequences

- The reuse check costs one hash per request, computed before the round trip.
  Negligible.
- Detection is only as good as the field list. Adding a billable field later
  means adding it to the fingerprint, and **existing rows will not have been
  hashed over it** — so reuse involving that field is undetectable for rows
  written before the change. Any future billable field needs this considered at
  the same time.
- A rejected reuse still performs the no-op `DO UPDATE` and so still writes a
  dead tuple (D18). The reuse path is expected to be rare enough not to matter.
- The stored event is never modified by a rejected reuse. A "rejection" that
  overwrote the original would be worse than accepting it, and there is a test
  asserting the original quantity survives.

## Open questions

- Two clients racing with the same key and different payloads: one wins, the
  other gets `409`. That is correct, but which one wins is arbitrary. Fine for
  a genuine bug; worth revisiting if it ever occurs legitimately.
- Whether `409` should include the stored fingerprint or a diff to help a
  client debug. Leaks nothing sensitive but adds surface area to the contract.
