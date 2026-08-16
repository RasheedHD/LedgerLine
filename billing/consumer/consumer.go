// Package consumer drains the broker log into Postgres.
//
// This is the second half of the seam. Ingest appends to the log and answers
// the client; this reads back from the log and makes the event durable in the
// database where billing can use it.
//
// See ADR-0009 for the delivery semantics and why the offset lives where it
// does.
package consumer

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/RasheedHD/LedgerLine/billing/event"
	brokerlog "github.com/RasheedHD/LedgerLine/broker/log"
)

// DefaultBatchSize is how many records are processed per transaction.
//
// One transaction per record would be simplest but pays a commit -- and an
// fsync inside Postgres -- for every event. Batching amortises that. The batch
// is still atomic, so a failure part-way rolls the whole batch back and it is
// retried from the same offset.
const DefaultBatchSize = 100

// insertEvent stores one event, ignoring one already present.
//
// The returned payload_fingerprint is the STORED one: the DO UPDATE touches
// only tenant_id, so every other column in the returned row still holds its
// original value. That is what makes a reused key detectable here.
const insertEvent = `
INSERT INTO events (tenant_id, meter, quantity, occurred_at, received_at, idempotency_key, payload_fingerprint)
VALUES ($1, $2, $3::numeric, $4, $5, $6, $7)
ON CONFLICT (tenant_id, idempotency_key)
DO UPDATE SET tenant_id = events.tenant_id
RETURNING (xmax = 0) AS inserted, payload_fingerprint`

// commitOffset advances this consumer's position.
const commitOffset = `
INSERT INTO consumer_offsets (consumer, next_offset, updated_at)
VALUES ($1, $2, $3)
ON CONFLICT (consumer)
DO UPDATE SET next_offset = EXCLUDED.next_offset, updated_at = EXCLUDED.updated_at`

const readOffset = `SELECT next_offset FROM consumer_offsets WHERE consumer = $1`

// insertDeadLetter records a record the consumer accepted but could not apply.
//
// ON CONFLICT DO NOTHING because a replay re-encounters every failed record.
// Without it each rebuild would append another copy and invariant I5 -- replay
// produces the same state -- would not hold.
const insertDeadLetter = `
INSERT INTO dead_letters (consumer, log_offset, reason, detail, tenant_id, idempotency_key, record, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (consumer, log_offset) DO NOTHING`

// Reasons a record ends up in the dead-letter table.
const (
	ReasonUndecodable = "undecodable_record"
	ReasonKeyReuse    = "idempotency_key_reuse"
)

// Consumer reads a log into the events table.
type Consumer struct {
	name string
	log  *brokerlog.Log
	db   *sql.DB
	opts Options
}

// Options configures a Consumer.
type Options struct {
	// BatchSize is how many records share one transaction. Zero means
	// DefaultBatchSize.
	BatchSize int

	// Logger receives warnings about events that could not be applied. Zero
	// means the default slog logger.
	Logger *slog.Logger
}

// Stats reports what a Drain did.
type Stats struct {
	// Read is how many records were taken off the log.
	Read int

	// Inserted is how many became new rows.
	Inserted int

	// Duplicates is how many were already present with matching content --
	// the expected result of a replay, and proof that reprocessing is safe.
	Duplicates int

	// Conflicts is how many carried a key already used for DIFFERENT content,
	// or could not be decoded at all. These do not become events; they are
	// written to the dead-letter table instead. Non-zero means usage was
	// dropped and a human needs to look.
	Conflicts int

	// DeadLettered is how many rows were written to dead_letters. Normally
	// equal to Conflicts; lower on a replay, where the rows already exist and
	// the insert is a no-op.
	DeadLettered int
}

// add accumulates one batch's stats into a running total.
//
// A method rather than four additions at the call site, because the field-by-
// field version is exactly the shape that silently drops a field when a new one
// is added -- which is what happened to DeadLettered, and the test that should
// have caught it passed instead because it asserted the count was zero.
func (s *Stats) add(batch Stats) {
	s.Read += batch.Read
	s.Inserted += batch.Inserted
	s.Duplicates += batch.Duplicates
	s.Conflicts += batch.Conflicts
	s.DeadLettered += batch.DeadLettered
}

// Accounted reports whether every record read was either stored, recognised as
// a duplicate, or dead-lettered.
//
// This is invariant I3 as an arithmetic identity. If it is ever false, a record
// came off the log and vanished without any of the three outcomes being
// recorded -- which is precisely the silent loss I3 forbids.
func (s Stats) Accounted() bool {
	return s.Inserted+s.Duplicates+s.Conflicts == s.Read
}

// New returns a consumer identified by name.
//
// The name is what the offset is stored against, so two consumers with the
// same name share a position and will fight over it.
func New(name string, l *brokerlog.Log, db *sql.DB, opts Options) *Consumer {
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Consumer{name: name, log: l, db: db, opts: opts}
}

// NextOffset returns the offset this consumer will read next.
func (c *Consumer) NextOffset(ctx context.Context) (uint64, error) {
	var next int64
	err := c.db.QueryRowContext(ctx, readOffset, c.name).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		// Never consumed anything. Start at the beginning of the log.
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read consumer offset: %w", err)
	}
	return uint64(next), nil
}

// Drain processes every record currently in the log and returns when it has
// caught up.
//
// Safe to call repeatedly, and safe to interrupt: an interrupted batch is
// rolled back and reprocessed from the same offset next time.
func (c *Consumer) Drain(ctx context.Context) (Stats, error) {
	var total Stats

	for {
		next, err := c.NextOffset(ctx)
		if err != nil {
			return total, err
		}

		end := c.log.NextOffset()
		if next >= end {
			return total, nil
		}

		batchEnd := next + uint64(c.opts.BatchSize)
		if batchEnd > end {
			batchEnd = end
		}

		stats, err := c.processBatch(ctx, next, batchEnd)
		total.add(stats)
		if err != nil {
			return total, err
		}
	}
}

// processBatch applies records [from, to) and advances the offset, atomically.
func (c *Consumer) processBatch(ctx context.Context, from, to uint64) (Stats, error) {
	var stats Stats

	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("begin batch: %w", err)
	}
	// Rollback after a successful Commit is a no-op, so this is safe as an
	// unconditional cleanup and removes the chance of an early return leaking
	// an open transaction.
	defer tx.Rollback()

	for offset := from; offset < to; offset++ {
		record, err := c.log.Read(offset)
		if err != nil {
			return stats, fmt.Errorf("read offset %d: %w", offset, err)
		}

		stats.Read++

		e, err := event.Decode(record)
		if err != nil {
			// A record that cannot be decoded will never decode. Failing here
			// would wedge the consumer on it forever, so it is dead-lettered
			// and stepped over. The raw bytes go with it, since "offset 412
			// failed" is not something anyone can act on.
			c.opts.Logger.Error("undecodable record dead-lettered",
				"offset", offset, "error", err)

			written, dlErr := c.deadLetter(ctx, tx, offset, ReasonUndecodable, err.Error(), nil, record)
			if dlErr != nil {
				return stats, fmt.Errorf("dead-letter offset %d: %w", offset, dlErr)
			}
			stats.Conflicts++
			if written {
				stats.DeadLettered++
			}
			continue
		}

		applied, err := c.apply(ctx, tx, e)
		if err != nil {
			return stats, fmt.Errorf("apply offset %d: %w", offset, err)
		}

		switch applied {
		case appliedInserted:
			stats.Inserted++
		case appliedDuplicate:
			stats.Duplicates++
		case appliedConflict:
			c.opts.Logger.Warn("idempotency key reused with different content; event dead-lettered",
				"offset", offset,
				"tenant_id", e.TenantID,
				"idempotency_key", e.IdempotencyKey)

			written, dlErr := c.deadLetter(ctx, tx, offset, ReasonKeyReuse,
				"idempotency key already used for an event with different content", e, record)
			if dlErr != nil {
				return stats, fmt.Errorf("dead-letter offset %d: %w", offset, dlErr)
			}
			stats.Conflicts++
			if written {
				stats.DeadLettered++
			}
		}
	}

	// CRITICAL: the offset commit is part of the same transaction as the
	// inserts above.
	//
	// This is what makes processing exactly-once. Committing the offset
	// separately -- after the inserts -- leaves a window where a crash means
	// the events are stored but the offset is not, and they are reprocessed.
	// Committing it before leaves the opposite window, where events are lost.
	// Inside one transaction there is no window at all.
	if _, err := tx.ExecContext(ctx, commitOffset, c.name, int64(to), time.Now().UTC()); err != nil {
		return stats, fmt.Errorf("commit offset: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("commit batch: %w", err)
	}
	return stats, nil
}

// deadLetter records a record that could not be applied, returning whether a
// new row was written.
//
// CRITICAL: this runs inside the batch's transaction, alongside the offset
// advance.
//
// If the dead letter were written separately, a crash between the two would
// either advance past a record with nothing recording that it failed -- silent
// loss, exactly what I3 forbids -- or record a failure for a record that was
// never actually skipped. Sharing the transaction removes both windows, for the
// same reason the offset itself lives in this database (ADR-0009).
func (c *Consumer) deadLetter(
	ctx context.Context,
	tx *sql.Tx,
	offset uint64,
	reason, detail string,
	e *event.UsageEvent,
	record []byte,
) (bool, error) {
	// Nil for an undecodable record, which by definition has no readable
	// tenant or key. sql.NullString rather than "" so the column is genuinely
	// NULL and the partial index skips it.
	var tenantID, idempotencyKey sql.NullString
	if e != nil {
		tenantID = sql.NullString{String: e.TenantID, Valid: true}
		idempotencyKey = sql.NullString{String: e.IdempotencyKey, Valid: true}
	}

	result, err := tx.ExecContext(ctx, insertDeadLetter,
		c.name, int64(offset), reason, detail, tenantID, idempotencyKey, record, time.Now().UTC())
	if err != nil {
		return false, err
	}

	// Zero rows means the conflict clause fired: this offset is already
	// recorded, which is the expected outcome on a replay.
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// DeadLetter is one unapplied record, as stored.
type DeadLetter struct {
	Offset         uint64
	Reason         string
	Detail         string
	TenantID       string
	IdempotencyKey string
	Record         []byte
	CreatedAt      time.Time
}

const selectUnresolvedDeadLetters = `
SELECT log_offset, reason, detail, COALESCE(tenant_id, ''), COALESCE(idempotency_key, ''), record, created_at
FROM dead_letters
WHERE consumer = $1 AND resolved_at IS NULL
ORDER BY log_offset
LIMIT $2`

// UnresolvedDeadLetters returns records this consumer could not apply and
// nobody has dealt with yet.
//
// The point of the dead-letter table is that this question has an answer. A
// non-empty result means usage was accepted, acknowledged to a client, and then
// dropped.
func (c *Consumer) UnresolvedDeadLetters(ctx context.Context, limit int) ([]DeadLetter, error) {
	rows, err := c.db.QueryContext(ctx, selectUnresolvedDeadLetters, c.name, limit)
	if err != nil {
		return nil, fmt.Errorf("read dead letters: %w", err)
	}
	defer rows.Close()

	var out []DeadLetter
	for rows.Next() {
		var d DeadLetter
		var offset int64
		if err := rows.Scan(&offset, &d.Reason, &d.Detail, &d.TenantID, &d.IdempotencyKey, &d.Record, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan dead letter: %w", err)
		}
		d.Offset = uint64(offset)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dead letters: %w", err)
	}
	return out, nil
}

type applyResult int

const (
	appliedInserted applyResult = iota
	appliedDuplicate
	appliedConflict
)

func (c *Consumer) apply(ctx context.Context, tx *sql.Tx, e *event.UsageEvent) (applyResult, error) {
	var inserted bool
	var stored []byte

	err := tx.QueryRowContext(ctx, insertEvent,
		e.TenantID,
		e.Meter,
		e.Quantity,
		e.OccurredAt,
		e.ReceivedAt,
		e.IdempotencyKey,
		e.Fingerprint,
	).Scan(&inserted, &stored)
	if err != nil {
		return 0, err
	}

	if inserted {
		return appliedInserted, nil
	}
	if stored != nil && !bytes.Equal(stored, e.Fingerprint) {
		return appliedConflict, nil
	}
	return appliedDuplicate, nil
}
