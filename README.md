# LedgerLine

[![CI](https://github.com/RasheedHD/LedgerLine/actions/workflows/ci.yml/badge.svg)](https://github.com/RasheedHD/LedgerLine/actions/workflows/ci.yml)

Event streaming and usage billing, written from scratch in Go. An append-only
log with its own segment format and crash recovery, feeding a double-entry
ledger — and a chaos suite that breaks the running system on purpose and checks
the invoices still come out right.

One third-party dependency: a Postgres driver. Everything else is the standard
library.

## The claim it exists to prove

> A message log can only promise **at-least-once delivery**. Exactly-once
> *delivery* is impossible across a network. But exactly-once *effect* is
> achievable, by making the processing side idempotent — and it can be
> demonstrated rather than asserted, by breaking the system and showing the
> invoices still balance.

Concretely, `chaos/` posts events while killing database connections, cancelling
the consumer mid-batch, rewinding its committed offset, replaying every record,
and interrupting a billing run mid-transaction. Then it closes the books and
asserts the invoice equals **one cent per acknowledged event**, computed
independently of anything the system stored.

## How it fits together

```
   client                                    HTTP request
     │
     │  POST /events
     ▼
┌─────────────────┐   validate, then append. 202 is returned only after
│ billing/ingest  │   an fsync that covers the record (group commit).
└────────┬────────┘   Never touches Postgres, so usage is still accepted
         │            while the database is down.
         ▼
┌─────────────────┐   Own segment format: length + CRC32C framing, sparse
│   broker/log    │   offset index, crash recovery that truncates a torn
└────────┬────────┘   tail. Reads are offset-addressed.
         │
         │  at-least-once  →  duplicates are CERTAIN, not hypothetical
         ▼
┌─────────────────┐   Advances its offset in the SAME transaction as the
│ billing/consumer│   rows it writes, so processing is exactly-once.
└────────┬────────┘   Anything it cannot apply goes to dead_letters.
         │
         ▼
┌─────────────────┐   Flat, graduated and volume pricing. A pure function:
│ billing/pricing │   no clock, no database, no map iteration order.
└────────┬────────┘
         │
         ▼
┌─────────────────┐   Double-entry. An unbalanced transaction cannot be
│ billing/ledger  │   expressed, and the database enforces balance too.
└────────┬────────┘
         │
         ▼
┌─────────────────┐   Periods, invoices, and the late-event roll-forward.
│billing/invoicing│   A closed invoice can never change.
└─────────────────┘
```

## The invariants

Everything in the design serves one of these. Each is enforced somewhere
specific, and the chaos suite asserts them together under injected faults.

| | Invariant | Enforced by |
|---|---|---|
| **I1** | Money is conserved — every transaction balances | Transfers that cannot express an imbalance, plus a deferred Postgres constraint trigger |
| **I2** | No usage is billed twice | `UNIQUE (tenant_id, idempotency_key)` and derived ledger idempotency keys |
| **I3** | No accepted usage is silently lost | `dead_letters` with the raw record, plus `Stats.Accounted()` as an arithmetic identity |
| **I4** | Closed invoices are immutable | `BEFORE UPDATE OR DELETE` triggers that raise unconditionally |
| **I5** | Replay is deterministic | Pure rating, sorted output, offset committed inside the write transaction |
| **I6** | No float touches money or quantity | A test that walks the AST of every file under `billing/` |

## Some things that turned out to be interesting

**A `kill -9` cannot produce a torn record.** Measured, not assumed: across
repeated runs the log's tail always ends on a clean record boundary, because the
kernel completes the `write` syscall before delivering the signal. Torn records
need power loss. ([ADR-0006](docs/adr/0006-segment-format.md))

**Durability per record is not expensive, it is prohibitive.** Measured on
256-byte payloads:

| Policy | Appends/sec | Durable on return? |
|---|---|---|
| No fsync | ~76,000 | No |
| fsync every append | **~312** | Yes |
| Group commit, 64 writers | **~2,211** | Yes |

`fsync`-per-append gains *nothing* from concurrency — 308/sec with 64 writers
against 312 with one — because its throughput is a property of the disk, not the
workload. Group commit reaches 7.2× at the same guarantee.
([ADR-0007](docs/adr/0007-index-and-durability.md),
[ADR-0008](docs/adr/0008-group-commit.md))

**The checksum covers the length field, not just the payload.** A flipped bit in
an unprotected length does not corrupt one record — it desynchronises the reader
from the entire stream. ([ADR-0006](docs/adr/0006-segment-format.md))

**Late usage rolls forward without a special case.** An event that arrives after
its own period closed is simply still unbilled, so the next billing run picks it
up. That falls out of the query rather than needing a rule.
([ADR-0015](docs/adr/0015-periods-and-invoices.md))

## Running it

Postgres and the migration tool:

```bash
docker compose up -d
go install -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@v4.19.1
migrate -path migrations -database "postgres://ledgerline:ledgerline@localhost:5432/ledgerline?sslmode=disable" up
```

Then the service, which runs both the HTTP endpoint and the consumer:

```bash
go run ./cmd/ingest
```

Send an event. Send it twice — the second is appended to the log again, because
ingest deliberately cannot deduplicate:

```bash
curl -X POST localhost:8080/events -H 'Content-Type: application/json' -d '{"tenant_id":"acme","meter":"api_calls","quantity":"1","occurred_at":"2026-08-03T11:00:00Z","idempotency_key":"demo-1"}'
```

Both requests return `202` with a log offset. Exactly one row appears in
`events`. That gap is the design.

## Tests

```bash
go test ./...           # everything, needs Postgres running
go test -short ./...    # skips the chaos suite
```

231 tests. Integration tests run against a real Postgres rather than mocks —
every correctness claim here lives in a constraint, a transaction, or a type,
and a mocked constraint proves nothing about whether the constraint exists.

They **skip** when no database is reachable, which keeps the local loop usable
and means a green local run can prove nothing. CI sets `LEDGERLINE_REQUIRE_DB`,
which turns that skip into a failure, and fails the build on any skipped test.

Load-bearing assertions are mutation-tested: the guard is deliberately broken to
confirm the test fails, then restored. A test that has never failed is untested.

## Layout

| Path | |
|---|---|
| `broker/log/` | Segment format, sparse index, crash recovery, group commit |
| `billing/ingest/` | HTTP handler, validation, append |
| `billing/consumer/` | Log → Postgres, exactly-once, dead letters |
| `billing/event/` | The wire type shared across the seam |
| `billing/pricing/` | Meters, plans, tiered pricing |
| `billing/ledger/` | Double-entry accounts, postings, balances |
| `billing/invoicing/` | Periods, invoices, revenue recognition |
| `chaos/` | Fault injection and invariant checks |
| `migrations/` | Six migrations, round-tripped in CI |
| `docs/adr/` | Seventeen decision records |

## Reading the decisions

Every design decision has an ADR in [docs/adr/](docs/adr/), including the ones
that turned out to be wrong and were superseded. [PLAN.md](PLAN.md) is the
project anchor: current state, phase roadmap, and a debt register of everything
deliberately left undone and why.

Worth starting with:

- [ADR-0001](docs/adr/0001-event-schema.md) — the event schema, two clocks, and
  who generates the idempotency key
- [ADR-0008](docs/adr/0008-group-commit.md) — why acknowledging durably is
  affordable only with group commit
- [ADR-0012](docs/adr/0012-ingest-appends-to-the-log.md) — trading an API
  guarantee for availability, and what that cost
- [ADR-0016](docs/adr/0016-chaos-suite.md) — the chaos suite, and the two bugs
  it found in itself first

## Scope

Deliberately absent: authentication, a UI, payment processing, currency
conversion, tax, and any form of distribution or replication. The log is
single-node on purpose — which is also why it cannot copy Kafka's default of not
fsyncing, since Kafka buys durability from replicas this system does not have.
