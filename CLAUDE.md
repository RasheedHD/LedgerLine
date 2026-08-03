# Working agreement

This is a portfolio project I'm using to learn distributed systems and
billing correctness. You write most of the code. My job is to understand
most lines well enough to defend them in a technical interview months from now.

## Priority

Build well-made software that stands up as a portfolio piece. Correctness,
tests, and clean structure come first.

**The teaching loop is paused, not deleted.** Don't stop to teach before
writing, and don't wait for a go-ahead on a plan. Build it, then tell me what
you built and why the load-bearing decisions went the way they did. I'll come
back to the tutoring mode later.

## Default loop for any new component

1. **Build it.** The code, its tests, and its ADR together in one pass.
2. **Walk me through it.** Section by section, explaining _why_ each decision
   was made, not just what the code does.
3. **Tell me what you'd do differently** with another day on it, and what you
   left as debt.

Still ask me when a decision is genuinely mine to make - a tradeoff with no
right answer, or something that contradicts an existing ADR. Don't ask about
things you can decide sensibly yourself.

## How to write code for me

- **No cleverness.** Boring, explicit, readable over idiomatic-and-dense.
  If there's a one-liner and a five-line version, write the five-line version.
- **No unexplained magic.** No struct embedding, reflection, generics,
  channels, or third-party libraries without telling me what they do first.
- **Comment the non-obvious.** Not `// increment i`. Comment _why_ a lock
  is there, why an error is swallowed, why the query is ordered.
- **Flag the load-bearing lines.** When a line is where correctness actually
  lives, mark it `// CRITICAL:` and explain it in extra detail.
- **Minimal dependencies.** Prefer the standard library. If you want to add
  a dependency, justify it and tell me what it would take to do without.

## The three components that matter most

These are the interview conversation. Everything else supports them, and they
deserve the most care, the best tests, and the clearest comments.

- billing/dedup/ - idempotency and deduplication
- billing/ledger/ - double-entry posting logic
- broker/log/ - segment format and offset index

## After every session

Append to docs/learning-log.md:

- What we built
- The key decision and the alternative we rejected
- One thing I couldn't have written myself yet

Write this from what actually happened in the session, in plain language,
and keep it under 10 lines. This is my interview revision material.

## Always

- Table-driven tests, and explain what each case is proving
- Every design decision gets an ADR in docs/adr/
- Suggest the commit message; keep commits small and honest
