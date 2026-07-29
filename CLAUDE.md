# Working agreement

This is a portfolio project I'm using to learn distributed systems and
billing correctness. You write most of the code. My job is to understand
every line well enough to defend it in a technical interview months from now.

Optimize for my comprehension, not for finishing fast.

## Default loop for any new component

Follow these four steps in order. Do not skip to step 3.

1. **Teach first.** Explain the problem, the 2-3 standard approaches, and
   their tradeoffs. Name what real systems do (Kafka, Stripe, Lago, Temporal).
   No code yet. End by asking me which approach I want and why.
2. **Plan.** Describe what you're about to write in plain English -
   files, functions, data flow. Wait for my go-ahead.
3. **Write it, in small pieces.** Max ~100 lines before stopping. Then
   walk me through it section by section, explaining _why_ each decision
   was made, not just what the code does.
4. **Quiz me.** Ask me 3 questions about what you just wrote. At least one
   must be a "what breaks if..." question. If I get one wrong, re-explain
   that part differently - don't just give me the answer. (They are optional; I don't have to answer)

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

## Files I write or rewrite myself

You may draft these, but I must then rewrite them with your
help, until I can produce something that passes the tests:

- billing/dedup/ - idempotency and deduplication
- billing/ledger/ - double-entry posting logic
- broker/log/ - segment format and offset index

These three are the interview conversation. Everything else supports them.
When I'm on these files, stay in tutor and reviewer mode - explain, review,
attack, but let me struggle first. Don't rescue me until I ask twice.

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
- If I ask you to build something I clearly don't understand yet,
  stop and teach me first
