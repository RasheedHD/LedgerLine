# ADR-0018: A distinct type for a log safe to acknowledge from

- **Status:** Accepted
- **Date:** 2026-08-19
- **Deciders:** Rasheed
- **Related:** [ADR-0008](0008-group-commit.md), [ADR-0012](0012-ingest-appends-to-the-log.md)
- **Resolves:** PLAN.md debt item D28

## Context

`SyncEveryN` acknowledges records that are not yet durable. With N = 1000,
records 1 through 999 return from `Append` with no `fsync` behind them.

That is fine where losing a bounded tail is acceptable, and it is a correctness
hole the moment anything answers a client on the strength of an append. Ingest
returns `202` after appending, so running it on that policy makes the `202` a
promise the system cannot keep across a power failure — invariant I3 broken by
construction.

The rule was recorded, in a doc comment on `NewHandler`:

> **CRITICAL:** the log passed here must use `brokerlog.SyncGroup`.

A comment stops nobody. Anyone wiring a second entry point, or tuning a policy
because a benchmark looked slow, would have to already know the rule to look for
it.

## Decision

**`ingest.NewHandler` takes a `*brokerlog.DurableLog`, obtainable only from
`OpenDurable`, which refuses a policy that acknowledges early.**

```go
func OpenDurable(dir string, opts Options) (*DurableLog, error)
```

`SyncPolicy.Durable()` is the predicate: true for `SyncAlways` and `SyncGroup`,
false for `SyncNever` and `SyncEveryN`.

The unsafe wiring no longer compiles:

```
cannot use l (variable of type *log.Log) as *log.DurableLog
value in argument to ingest.NewHandler
```

That is the entire point. The rule moved from something a reader has to know to
something the compiler enforces.

### Why a wrapper type rather than a runtime check

A runtime check — `NewHandler` returning an error on a non-durable log — would
also catch it, and would catch it *after* the program had started, on a code
path someone may not exercise before shipping. A type mismatch is found by
`go build`.

`DurableLog` embeds `*Log`, which promotes every method — `Append`, `Read`,
`Close` and the rest — so a `DurableLog` is usable anywhere a `Log` is without
wrapping each method by hand. Embedding is the one piece of Go magic in this
package and it is doing exactly one job: type identity, with behaviour
inherited.

### It is a guardrail, not a prison

The inner log is reachable as `.Log`, and several call sites use it: the
consumer takes a plain `*Log` because it reads the log and acknowledges nobody,
so the durable guarantee is irrelevant to it.

That is deliberate. The goal is to make the unsafe path **deliberate** rather
than impossible — someone who genuinely wants a non-durable log for a component
that does not acknowledge can have one, and the `.Log` at the call site is a
visible marker that they chose it.

### `SyncEveryN` stays

Deleting it would close the hole outright. It stays because it is a legitimate
policy for a caller that does not acknowledge, and because the measured
comparison in ADR-0007 — where it sits between "no fsync" and "fsync per
append" — is part of what the benchmark is for. A dial with one setting removed
because it was misusable is a worse explanation than a dial that cannot be
misused where it matters.

## Verified

- A probe program passing a `SyncEveryN` log to `NewHandler` fails to compile,
  with the error above. Before this change it compiled and ran.
- `OpenDurable` refuses `SyncNever` and `SyncEveryN` and accepts `SyncAlways`
  and `SyncGroup`, table-tested, with the error required to name the policy so
  an operator reading a startup failure can tell which setting was wrong.
- A `DurableLog` appends, reads and reports offsets through the embedded type,
  so nothing else had to change to accept one.

## Consequences

- One more type in the log's public surface, and one more thing to explain. The
  cost is real; the alternative is a comment that has already been shown to be
  the weakest possible enforcement.
- Call sites that legitimately want a plain log now write `.Log`, which reads
  slightly worse and says something true.
- The same pattern would suit other "this component requires that guarantee"
  relationships if any appear. None have yet, and inventing them speculatively
  would be worse than the comment this replaces.

## Open questions

- `Options.Sync` is still a plain enum, so `OpenDurable` validates at runtime
  what a narrower parameter type could refuse at compile time. Splitting the
  enum would push the check one step earlier and make `Options` harder to
  construct for the common case; the current split lands the compile-time
  guarantee where the actual danger is.
