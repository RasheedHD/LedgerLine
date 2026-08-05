# ADR-0006: Segment format and record framing

- **Status:** Accepted
- **Date:** 2026-08-03
- **Deciders:** Rasheed
- **Related:** [PLAN.md](../../PLAN.md) Phase 2

## Context

`broker/log/` is an append-only log addressed by offset. Before anything can
be written on top of it, three things have to be decided: how a single record
is framed on disk, how records are grouped into files, and what happens when a
process dies partway through writing one.

Framing is the decision everything else inherits. It cannot be changed later
without a migration of every existing log file.

## Decision

### Record framing

```
+----------+----------+------------------+
| length   | crc32c   | payload          |
| 4 bytes  | 4 bytes  | `length` bytes   |
+----------+----------+------------------+
```

Big-endian, which is arbitrary but must never change: it is the difference
between reading a log written by an older build and reading garbage.

**The checksum covers the length field, not just the payload.** This is the
load-bearing detail.

Checksumming only the payload leaves the length unprotected, and the length is
what tells a reader how many bytes to consume. A flipped bit there does not
corrupt one record — it desynchronises the reader from the record stream, so
every subsequent record in the segment is read from the wrong position and the
damage silently spreads forward. Covering the length makes the failure local
and detectable at the record where it happened.

There is a test that fails if the length is excluded from the checksum.

**A length beyond `MaxRecordSize` (8 MiB) is rejected before allocating.** A
corrupted length field is otherwise an instruction to allocate an arbitrary
amount of memory, which converts disk corruption into an out-of-memory crash.

**Castagnoli (CRC-32C) rather than the IEEE polynomial.** Hardware support on
every CPU this will realistically run on, and it is what Kafka, RocksDB, and
LevelDB use for the same job.

### Segment files

```
+---------+-----------+-------------+--------------+
| magic   | version   | baseOffset  | records...   |
| 4 bytes | 4 bytes   | 8 bytes     |              |
+---------+-----------+-------------+--------------+
```

Named `%020d.log` by base offset, zero-padded so lexical filename order matches
numeric offset order — which is what makes listing a directory sufficient to
recover segment sequence.

The magic number is checked on open so that pointing the log at the wrong
directory fails loudly, rather than being interpreted as a corrupt segment and
**truncated to nothing**. That failure mode is why the check exists: without
it, an operator error becomes silent data destruction. The version field makes
a future format change a readable error instead of silent misparsing.

Segments roll at 64 MiB so that old data can be dropped by unlinking a file
rather than rewriting a large one, and so recovery scans only the tail of the
log.

`O_EXCL` on creation, so reopening and appending to an existing segment is
impossible. That would corrupt the offset sequence in a way no checksum could
catch, because every individual record would still be valid.

### Recovery

On open, every segment is scanned forward from its header. The scan stops at
the first record that fails to read, and **the file is truncated there**.

Truncating rather than skipping matters: if the damaged bytes stay on disk, the
next append writes after garbage and the corruption becomes permanent.

The assumption this makes explicit: **damage is at the tail.** Writes are
sequential, so a crash can only leave a partial record at the end. Bit rot in
the middle of a segment is detected by the checksum, but truncating there
discards every valid record after it. That is the same tradeoff Kafka makes,
and it is a real limitation rather than an oversight — there is a test pinning
down exactly what happens.

## What the crash test actually proved

The plan called for a `kill -9` mid-append test. It exists, kills a child
process writing 64 KiB records, and asserts the log reopens with every record
intact.

**Measured result: the tail is never actually torn.** Across repeated runs, the
segment always ends on a clean record boundary. Each record is written with a
single `WriteAt`; the kernel completes that syscall before delivering the kill,
and Go retries short writes internally. A process cannot be interrupted partway
through its own write.

This is worth stating plainly because it is easy to assume otherwise:

- **Process death** cannot produce a torn record. The page cache belongs to the
  kernel and outlives the process.
- **Power loss** can, because the machine stops between the kernel accepting a
  write and the disk committing it.

So the crash test proves a killed writer never leaves an unreadable log, and
`TestRecoversFromTornTail` — which constructs the damage directly — is the only
thing that exercises the repair path. The two cover genuinely different
failures rather than duplicating each other. Asserting that a tear *must* occur
would make the crash test fail for the right reason on a correct system.

## Consequences

- `Append` does not `fsync`. On return a record has been handed to the
  operating system, which survives this process dying but not the machine
  losing power. The durability policy is deliberately left to its own ADR,
  which should carry measured numbers rather than a repeated claim about what
  Kafka does. **Until then the log's durability guarantee is "process-crash
  safe, not power-loss safe."**
- The in-memory position table is **dense** — 8 bytes per record, roughly
  800 MB at a hundred million records. That cost is exactly why Kafka keeps a
  sparse index on disk, and replacing it is the next step in Phase 2. It also
  forces a full scan of every segment on open, not just the active one.
- `MaxRecordSize` of 8 MiB is a hard ceiling on a single event. Comfortable for
  usage events; would need revisiting for anything batched.

## Open questions

- Should a corrupt record mid-segment really truncate, or should it be skipped
  so later records survive? Skipping means offsets and file positions disagree,
  which the index would have to encode. Deferred until there is a reason to
  care.
- Segment deletion and retention are unimplemented; the log currently grows
  without bound.
