# ADR-0003: pgx as the Postgres driver, used through database/sql

- **Status:** Accepted
- **Date:** 2026-07-29
- **Deciders:** Rasheed
- **Related:** [ADR-0001](0001-event-schema.md)

## Context

`POST /events` needs to talk to Postgres. This is the project's first
third-party dependency, so it is worth being explicit about why it exists.

`database/sql` is standard library, but it is only an interface — it ships
with no drivers. Doing without one entirely means implementing the Postgres
wire protocol, including the extended query protocol and binary type
encoding. That is a serious project in its own right and would teach nothing
this project is about. One driver dependency is unavoidable; the decision is
which, and how tightly to couple to it.

## Decision

**`github.com/jackc/pgx/v5`, imported for side effects only and used entirely
through `database/sql`.**

```go
import _ "github.com/jackc/pgx/v5/stdlib"

db, err := sql.Open("pgx", dsn)
```

### Options considered

**`lib/pq`.** The older pure-Go driver, and the one most tutorials use. Its
README declares it in maintenance mode — it accepts bug fixes but is not
actively developed. It also hands back `NUMERIC` as raw bytes for the caller
to interpret. Given ADR-0001 §3 is entirely about not losing decimal
precision, a driver with weak numeric handling is the wrong foundation, even
though it was already in the module cache from installing `golang-migrate`
and would have cost nothing to adopt.

**`pgx` native API, no `database/sql`.** Best performance and the richest type
support. Rejected because it means learning a library-specific API instead of
the interface every other Go codebase uses. For a project whose purpose is to
be explainable in an interview, transferable knowledge beats a benchmark.

**`pgx` via the `stdlib` adapter.** Chosen. Actively maintained, proper
`NUMERIC` support, and every line of application code is standard library
API — so the driver is one import line away from being swapped, and nothing
in `billing/ingest` knows which database it is talking to.

## Consequences

- One direct dependency, four transitive (`pgpassfile`, `pgservicefile`,
  `puddle`, `golang.org/x/text`). All are pgx's own or `x/` repos.
- No Go decimal library is needed. Quantity stays a `string` from the wire
  into the `$3::numeric` placeholder and is never a Go number, so
  `shopspring/decimal` and its arithmetic surface never enter the project.
  The precision guarantee lives in exactly one place: the column type.
- The `stdlib` adapter costs some performance versus native pgx. Irrelevant
  at current volumes; revisit if the broker's throughput work needs `COPY` or
  pipelined batching, both of which require dropping to the native API.
- `sql.Open` is lazy and does not connect, so startup must `Ping` explicitly
  or a dead database first appears as a failing request rather than a failing
  boot. Handled in `cmd/ingest/main.go`.

## Open questions

- Connection pool limits (`SetMaxOpenConns`, `SetMaxIdleConns`,
  `SetConnMaxLifetime`) are all at their defaults, which means unbounded open
  connections. Fine for one laptop, wrong for anything with concurrency.
  Needs sizing against Postgres's own `max_connections` before load testing.
