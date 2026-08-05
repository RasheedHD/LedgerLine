package log

import "sync"

// syncCoordinator implements group commit: many concurrent appends wait on one
// shared fsync instead of each paying for its own.
//
// The problem it solves is the measurement in ADR-0007. An fsync costs about
// 3.2 ms, so acknowledging every append behind its own sync caps the log at
// roughly 312 appends/sec no matter how many clients are waiting. But
// acknowledging *without* a sync means promising durability the log cannot
// deliver, which breaks invariant I3 the moment ingest returns 202 on the
// strength of an append.
//
// Group commit escapes the choice. One fsync flushes everything written before
// it, so if fifty writers have appended and all are waiting, a single sync makes
// all fifty durable. Throughput then scales with concurrency while every
// acknowledgement is still backed by a completed sync. Databases have done this
// since the 1980s; it is the same idea as a commit log group commit in Postgres
// or InnoDB.
//
// The sequencing rule that makes it correct: a syncer captures the highest
// written sequence BEFORE calling fsync, and claims only that much afterwards.
// Records written while the sync is in flight may or may not have been caught
// by it, so they are left for the next round rather than assumed durable.
type syncCoordinator struct {
	mu   sync.Mutex
	cond *sync.Cond

	// Every sequence at or below synced is on disk.
	synced uint64

	// The highest sequence written to the file, durable or not.
	pending uint64

	// True while some goroutine is inside fsync. Others queue rather than
	// starting a second one.
	syncing bool

	// Set when a sync fails, so waiters learn about it rather than blocking
	// forever. Cleared when the next sync attempt begins.
	err error

	// Count of fsync calls actually made, so tests and benchmarks can show
	// batching is really happening rather than assuming it.
	syncs uint64
}

func newSyncCoordinator() *syncCoordinator {
	c := &syncCoordinator{}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// waitFor blocks until sequence seq is durable, performing the fsync itself if
// nobody else is already doing one.
//
// doSync is passed in rather than stored so the coordinator stays independent
// of which segment is being flushed.
func (c *syncCoordinator) waitFor(seq uint64, doSync func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if seq > c.pending {
		c.pending = seq
	}

	for {
		if c.synced >= seq {
			return nil
		}
		if c.err != nil {
			return c.err
		}

		if c.syncing {
			// Someone else is flushing. Wait to be woken, then re-check --
			// their sync may or may not have covered this sequence.
			c.cond.Wait()
			continue
		}

		// Become the syncer for this round.
		c.syncing = true
		c.err = nil

		// CRITICAL: capture the target before releasing the lock. Anything
		// appended while the fsync is in flight is NOT claimed by it, because
		// there is no guarantee fsync caught a write that landed after it
		// started. Claiming those would mark records durable that are not.
		target := c.pending
		c.syncs++

		c.mu.Unlock()
		err := doSync()
		c.mu.Lock()

		c.syncing = false
		if err != nil {
			c.err = err
			// Wake everyone: a failing disk is not something to wait out.
			c.cond.Broadcast()
			return err
		}
		if target > c.synced {
			c.synced = target
		}
		c.cond.Broadcast()

		// Loop rather than returning: if this writer's sequence was above the
		// captured target, it needs another round.
	}
}

// markSynced records that everything up to seq is durable without performing a
// sync, for when the caller already flushed by another route -- notably a
// segment roll, which syncs the outgoing segment while holding the log lock.
func (c *syncCoordinator) markSynced(seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if seq > c.synced {
		c.synced = seq
	}
	if seq > c.pending {
		c.pending = seq
	}
	c.cond.Broadcast()
}

// syncCount reports how many fsync calls have been made.
func (c *syncCoordinator) syncCount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.syncs
}
