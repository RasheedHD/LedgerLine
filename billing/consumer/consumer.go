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

	// Conflicts is how many carried a key already used for DIFFERENT content.
	// These are not stored. Non-zero means a client reused an idempotency key,
	// and the usage in those events has been dropped.
	Conflicts int
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
		total.Read += stats.Read
		total.Inserted += stats.Inserted
		total.Duplicates += stats.Duplicates
		total.Conflicts += stats.Conflicts
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

		e, err := event.Decode(record)
		if err != nil {
			// A record that cannot be decoded will never decode. Failing here
			// would wedge the consumer on it forever, so it is counted,
			// reported, and stepped over. Invariant I3 says nothing may be
			// lost SILENTLY -- this is loud.
			c.opts.Logger.Error("undecodable record skipped",
				"offset", offset, "error", err)
			stats.Read++
			stats.Conflicts++
			continue
		}

		applied, err := c.apply(ctx, tx, e)
		if err != nil {
			return stats, fmt.Errorf("apply offset %d: %w", offset, err)
		}

		stats.Read++
		switch applied {
		case appliedInserted:
			stats.Inserted++
		case appliedDuplicate:
			stats.Duplicates++
		case appliedConflict:
			stats.Conflicts++
			c.opts.Logger.Warn("idempotency key reused with different content; event dropped",
				"offset", offset,
				"tenant_id", e.TenantID,
				"idempotency_key", e.IdempotencyKey)
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
