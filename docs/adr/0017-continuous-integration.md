# ADR-0017: Continuous integration, and refusing to skip

- **Status:** Accepted
- **Date:** 2026-08-19
- **Deciders:** Rasheed
- **Resolves:** PLAN.md debt items D16, D19, D43

## Context

The project had no CI, and two consequences of that were worse than the absence
itself.

**Integration tests skip silently without a database.** That is right for local
work — `go test ./...` stays useful when Docker is not running — but it means a
green run can prove nothing at all. It has already happened repeatedly during
development: a suite reporting `ok` with every database test skipped.

**`go test -race` has never run.** Not once, on any code in this repository. The
development machine's gcc is 32-bit only and cannot build the race runtime
(*"sorry, unimplemented: 64-bit mode not compiled in"*). Group commit in
`broker/log` is the most concurrency-sensitive code in the project and has never
been near a detector.

## Decision

**GitHub Actions on Linux, with a Postgres 16 service container, running
`go test -race` over everything including the chaos suite.**

### LEDGERLINE_REQUIRE_DB turns a skip into a failure

The environment variable is read by `internal/testdb`. When it is set, a
database that cannot be reached is a `t.Fatalf` rather than a `t.Skipf`.

This is the load-bearing part of the whole workflow. Without it, a broken
service container produces a **passing** build with every integration test
quietly skipped — and that is strictly worse than a red build, because the badge
then claims the invariants hold when nothing checked them. A green CI that
proves nothing is an active lie; a red one is merely bad news.

Verified both ways: pointed at a dead port, the suite reports `ok` without the
flag and fails with it.

There is a belt-and-braces second gate as well — a step that greps the verbose
output for skipped tests and fails on any hit. The environment variable covers
the database case; the grep covers a test skipping for some reason nobody has
thought of yet.

### CI applies no migrations

`internal/testdb` drops and replays everything in `migrations/` itself. If CI
applied the schema instead, the tests would run against whatever CI set up
rather than against what the migrations describe, and a broken migration could
pass unnoticed.

A separate step round-trips the migrations with `golang-migrate` — up, down to
zero, up again. That is a different claim from "the schema is right" and worth
checking on its own.

### The chaos suite runs in CI but not in the fast loop

`-short` skips the seven chaos scenarios, which take around twenty seconds
together and considerably longer under `-race`. CI does not pass `-short`. That
resolves D43 without giving up the coverage where it matters.

### .gitattributes normalises line endings

Development is on Windows, CI is on Linux. Every commit so far has warned about
LF/CRLF conversion, and a stray CRLF in a Go file would fail the formatting gate
for a reason with nothing to do with the code.

## What this does not yet prove

**The race detector has still never run.** The workflow says it should; the
first push is the first time it will.

It may well fail. Group commit deliberately releases a lock around an fsync and
coordinates waiters through a condition variable, which is exactly the shape a
detector is built to find problems in.

D21 is therefore recorded as **addressed, not closed**. It closes when a run
comes back green — and if it comes back red, that is the workflow doing its job
on its first attempt.

## Consequences

- CI needs a Postgres container on every run, so the pipeline is slower and more
  fragile than a pure unit suite. That is the cost of testing against a real
  database, and the reasoning from `internal/testdb` applies: a mocked
  constraint proves nothing about whether the constraint exists.
- `-race` roughly triples the suite runtime. Acceptable while the whole thing is
  about a minute.
- Nothing runs the benchmarks in CI, so a performance regression in the log
  would go unnoticed. Benchmarks are noisy on shared runners, and a number that
  cannot be trusted is worse than no number (**D46**).

## Open questions

- Should CI fail on a formatting diff, or fix it and push? Failing is chosen,
  since a pipeline that rewrites the branch it is testing is a surprise nobody
  wants.
- A matrix over Go versions would catch a standard library behaviour change, at
  the cost of a slower pipeline for a single-developer project pinned to one
  toolchain.
