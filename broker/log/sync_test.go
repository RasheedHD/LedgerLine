package log

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// Every appended record must be durable by the time Append returns. This is
// the property that lets ingest answer 202 on the strength of a log append
// without breaking invariant I3.
func TestGroupCommitSyncsBeforeReturning(t *testing.T) {
	l := openTestLogWith(t, t.TempDir(), Options{Sync: SyncGroup})

	for i := 0; i < 20; i++ {
		before := l.SyncCount()
		if _, err := l.Append([]byte(fmt.Sprintf("record-%d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if l.SyncCount() <= before {
			t.Fatalf("append %d returned without an fsync behind it", i)
		}
	}
}

// The point of group commit: with many writers, one fsync should cover many
// records. Without batching this test would show one sync per append, which is
// SyncAlways wearing a different name.
func TestGroupCommitBatchesConcurrentAppends(t *testing.T) {
	l := openTestLogWith(t, t.TempDir(), Options{Sync: SyncGroup})

	const writers = 64
	const perWriter = 20
	const total = writers * perWriter

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if _, err := l.Append([]byte(fmt.Sprintf("w%02d-%02d", w, i))); err != nil {
					t.Errorf("append: %v", err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	syncs := l.SyncCount()
	if syncs == 0 {
		t.Fatal("no fsync happened at all; records were acknowledged without being durable")
	}
	if syncs >= total {
		t.Fatalf("%d syncs for %d appends -- no batching occurred", syncs, total)
	}
	if got := l.NextOffset(); got != total {
		t.Fatalf("NextOffset = %d, want %d", got, total)
	}

	t.Logf("%d appends needed %d fsyncs (%.1f records per sync)",
		total, syncs, float64(total)/float64(syncs))
}

// A sync that fails must reach the writers waiting on it. Silently returning
// success would hand out acknowledgements for records that never reached disk.
func TestGroupCommitPropagatesSyncFailure(t *testing.T) {
	c := newSyncCoordinator()
	wantErr := errors.New("disk on fire")

	var wg sync.WaitGroup
	errs := make([]error, 8)

	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = c.waitFor(uint64(i+1), func() error { return wantErr })
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if !errors.Is(err, wantErr) {
			t.Errorf("waiter %d got %v, want the sync error", i, err)
		}
	}
}

// A syncer claims only what was written before its fsync began. Records that
// arrive mid-flush must wait for the next round rather than being marked
// durable by a sync that may not have caught them.
func TestGroupCommitDoesNotClaimRecordsWrittenDuringFlush(t *testing.T) {
	c := newSyncCoordinator()

	started := make(chan struct{})
	release := make(chan struct{})

	// Writer 1 becomes the syncer and blocks inside the fsync.
	go func() {
		_ = c.waitFor(1, func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	// Writer 2 arrives while that flush is in progress.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.waitFor(2, func() error { return nil })
	}()

	// Let the first flush finish. It captured a target of 1, so it must not
	// mark sequence 2 durable.
	close(release)
	<-done

	c.mu.Lock()
	synced := c.synced
	syncs := c.syncs
	c.mu.Unlock()

	if synced < 2 {
		t.Fatalf("synced = %d, want at least 2 once the second writer returned", synced)
	}
	if syncs < 2 {
		t.Errorf("only %d fsyncs; the record written during the flush was covered by it, which is not safe to assume", syncs)
	}
}

// Records that land in a segment before a roll are flushed as part of rolling,
// so a later group sync of the new active segment cannot leave them behind.
func TestGroupCommitSurvivesSegmentRoll(t *testing.T) {
	dir := t.TempDir()
	l := openTestLogWith(t, dir, Options{Sync: SyncGroup, MaxSegmentBytes: 300})

	const total = 200
	for i := 0; i < total; i++ {
		if _, err := l.Append([]byte(fmt.Sprintf("record-%04d", i))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened := openTestLogWith(t, dir, Options{Sync: SyncGroup, MaxSegmentBytes: 300})
	if got := reopened.NextOffset(); got != total {
		t.Fatalf("NextOffset after reopen = %d, want %d -- records were lost across a roll", got, total)
	}
	for i := 0; i < total; i++ {
		if _, err := reopened.Read(uint64(i)); err != nil {
			t.Fatalf("read %d after reopen: %v", i, err)
		}
	}
}
