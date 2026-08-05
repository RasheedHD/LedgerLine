package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// DefaultMaxSegmentBytes is the size at which a segment rolls.
//
// Segments exist so that old data can be deleted by unlinking a whole file
// rather than rewriting a large one, and so that recovery only has to scan the
// tail of the log rather than all of it. 64 MiB is small enough that a scan is
// quick and large enough that rolling is rare.
const DefaultMaxSegmentBytes = 64 << 20

// SyncPolicy decides when Append forces data to durable storage.
//
// This is the durability/throughput dial, and there is no free setting. See
// ADR-0007 for the measured cost of each.
type SyncPolicy int

const (
	// SyncNever leaves flushing to the operating system. Records survive this
	// process being killed, because the page cache belongs to the kernel, but
	// not the machine losing power. This is Kafka's default, and Kafka gets
	// away with it by having replicas on other machines -- which this log does
	// not.
	SyncNever SyncPolicy = iota

	// SyncAlways fsyncs on every append. A record is on disk before Append
	// returns, so an acknowledged write survives power loss. Costs roughly two
	// orders of magnitude in throughput.
	SyncAlways

	// SyncEveryN fsyncs once per N appends, bounding how much can be lost to
	// power failure without paying per record. The exposure is up to N records.
	//
	// Note carefully: this acknowledges records that are not yet durable. With
	// N = 1000, records 1 through 999 return from Append with no sync behind
	// them. It is safe where losing a bounded tail is acceptable, and unsafe as
	// the basis for acknowledging a client. Use SyncGroup for that.
	SyncEveryN

	// SyncGroup makes concurrent appends wait on one shared fsync. Every
	// acknowledgement is backed by a completed sync, as with SyncAlways, but
	// the cost is divided among everyone flushing together, so throughput rises
	// with concurrency instead of being capped by one fsync per record.
	//
	// This is the policy an acknowledging API wants. See ADR-0008.
	SyncGroup
)

// Options configures a Log.
type Options struct {
	// MaxSegmentBytes is the size at which the active segment is closed and a
	// new one started. Zero means DefaultMaxSegmentBytes.
	MaxSegmentBytes int64

	// Sync selects the durability policy. Zero value is SyncNever.
	Sync SyncPolicy

	// SyncEveryNRecords is the batch size for SyncEveryN. Zero means
	// DefaultSyncEveryNRecords.
	SyncEveryNRecords int
}

// DefaultSyncEveryNRecords bounds power-loss exposure to a few hundred records
// under SyncEveryN.
const DefaultSyncEveryNRecords = 100

// Log is an append-only sequence of records addressed by offset.
//
// Offsets start at zero and increase by one per record, with no gaps. They are
// assigned by the log, never by the caller.
type Log struct {
	// Appends mutate the active segment's size, offset counter, and position
	// table, and may swap the active segment entirely. Reads only consult
	// those, so an RWMutex lets concurrent readers proceed while still
	// serialising the one writer against them.
	mu sync.RWMutex

	dir  string
	opts Options

	// Ordered by baseOffset. The last element is the active segment, the only
	// one that accepts writes.
	segments []*segment

	// Counts appends since the last fsync, for SyncEveryN.
	sinceSync int

	// Monotonic sequence per append, used to order writers against syncs.
	seq uint64

	// Coordinates group commit. Always non-nil; unused under other policies.
	syncer *syncCoordinator
}

// Open opens the log in dir, creating the directory and a first segment if
// they do not exist.
//
// Every existing segment is scanned on open, which both repairs a torn tail
// and rebuilds the in-memory position tables that reads depend on. Only the
// last segment can be damaged -- writes are sequential -- but all of them must
// be read to know where their records are. That full scan is a consequence of
// the dense in-memory index and goes away with the sparse on-disk one.
func Open(dir string, opts Options) (*Log, error) {
	if opts.MaxSegmentBytes <= 0 {
		opts.MaxSegmentBytes = DefaultMaxSegmentBytes
	}
	if opts.SyncEveryNRecords <= 0 {
		opts.SyncEveryNRecords = DefaultSyncEveryNRecords
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	baseOffsets, err := listSegmentOffsets(dir)
	if err != nil {
		return nil, err
	}

	l := &Log{dir: dir, opts: opts, syncer: newSyncCoordinator()}

	for _, baseOffset := range baseOffsets {
		s, err := openSegment(filepath.Join(dir, segmentName(baseOffset)))
		if err != nil {
			l.Close()
			return nil, err
		}
		l.segments = append(l.segments, s)
	}

	if len(l.segments) == 0 {
		s, err := createSegment(dir, 0)
		if err != nil {
			return nil, err
		}
		l.segments = append(l.segments, s)
	}

	return l, nil
}

// listSegmentOffsets returns the base offsets of every segment in dir, sorted.
func listSegmentOffsets(dir string) ([]uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read log directory: %w", err)
	}

	var offsets []uint64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".log") {
			continue
		}

		// The filename is the authority on a segment's base offset, and it is
		// also written into the header. A mismatch would mean the directory
		// has been tampered with, which openSegment would not catch, so the
		// name is parsed strictly rather than guessed at.
		base, err := strconv.ParseUint(strings.TrimSuffix(name, ".log"), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unexpected file %q in log directory: %w", name, err)
		}
		offsets = append(offsets, base)
	}

	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	return offsets, nil
}

// Append stores payload and returns the offset it was written at.
//
// What durability the returned offset carries depends entirely on Options.Sync:
//
//   - SyncNever and SyncEveryN: the record has been handed to the operating
//     system. It survives this process dying but not the machine losing power.
//     SyncEveryN in particular returns for most records with no sync behind
//     them at all.
//   - SyncAlways and SyncGroup: the record is on disk before this returns.
func (l *Log) Append(payload []byte) (uint64, error) {
	l.mu.Lock()

	active := l.segments[len(l.segments)-1]

	// Roll before writing rather than after, so a record is never split across
	// segments and a segment never exceeds its bound. The record-count check
	// keeps a single record larger than the threshold from producing an
	// endless sequence of empty segments -- it goes into a fresh segment on
	// its own and overshoots, which is the lesser problem.
	recordSize := int64(recordHeaderSize + len(payload))
	if active.nextOffset > active.baseOffset && active.size+recordSize > l.opts.MaxSegmentBytes {
		// The outgoing segment is flushed as part of rolling, so that group
		// commit only ever has to think about the active one. Without this a
		// waiter whose record landed in a previous segment could be woken by a
		// sync that never touched the file its record is in.
		if l.opts.Sync != SyncNever {
			if err := active.sync(); err != nil {
				l.mu.Unlock()
				return 0, fmt.Errorf("sync on segment roll: %w", err)
			}
			l.syncer.markSynced(l.seq)
		}

		next, err := createSegment(l.dir, active.nextOffset)
		if err != nil {
			l.mu.Unlock()
			return 0, err
		}
		l.segments = append(l.segments, next)
		active = next
	}

	offset, err := active.append(payload)
	if err != nil {
		l.mu.Unlock()
		return 0, err
	}

	// Sequence numbers order appends for the sync coordinator. Distinct from
	// offsets only because they must keep counting across segment boundaries
	// without being confused with the log's addressing.
	l.seq++
	seq := l.seq

	l.sinceSync++
	sinceSync := l.sinceSync
	if l.opts.Sync == SyncEveryN && sinceSync >= l.opts.SyncEveryNRecords {
		l.sinceSync = 0
	}

	// CRITICAL: the lock is released before any fsync.
	//
	// Group commit only works if other writers can append while one of them is
	// flushing -- the batch a single sync covers is exactly the set of writers
	// that arrived during the previous flush. Holding the log lock across the
	// sync would serialise appends behind it and reduce group commit to
	// SyncAlways with extra machinery.
	l.mu.Unlock()

	switch l.opts.Sync {
	case SyncAlways:
		if err := l.syncActive(); err != nil {
			return 0, fmt.Errorf("sync after append: %w", err)
		}
	case SyncEveryN:
		if sinceSync >= l.opts.SyncEveryNRecords {
			if err := l.syncActive(); err != nil {
				return 0, fmt.Errorf("sync after append: %w", err)
			}
		}
	case SyncGroup:
		if err := l.syncer.waitFor(seq, l.syncActive); err != nil {
			return 0, fmt.Errorf("group sync after append: %w", err)
		}
	}

	return offset, nil
}

// syncActive flushes the segment currently accepting writes.
func (l *Log) syncActive() error {
	l.mu.RLock()
	if len(l.segments) == 0 {
		l.mu.RUnlock()
		return nil
	}
	active := l.segments[len(l.segments)-1]
	l.mu.RUnlock()

	return active.sync()
}

// SyncCount reports how many fsync calls group commit has actually made.
//
// Exposed so tests and benchmarks can demonstrate batching rather than assume
// it: with group commit under load this should be far below the number of
// appends.
func (l *Log) SyncCount() uint64 {
	return l.syncer.syncCount()
}

// Read returns the payload stored at offset.
func (l *Log) Read(offset uint64) ([]byte, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	// sort.Search finds the first segment whose base offset is greater than
	// the one wanted; the segment before it is the one that holds the record.
	i := sort.Search(len(l.segments), func(i int) bool {
		return l.segments[i].baseOffset > offset
	})
	if i == 0 {
		return nil, fmt.Errorf("%w: %d is before the start of the log", ErrOffsetOutOfRange, offset)
	}

	return l.segments[i-1].read(offset)
}

// NextOffset returns the offset the next appended record will receive, which
// is also the count of records in the log.
func (l *Log) NextOffset() uint64 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.segments[len(l.segments)-1].nextOffset
}

// Sync forces every segment's writes to durable storage.
func (l *Log) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, s := range l.segments {
		if err := s.sync(); err != nil {
			return fmt.Errorf("sync segment %d: %w", s.baseOffset, err)
		}
	}
	return nil
}

// Close closes every segment.
//
// Errors from individual segments are collected rather than returned on the
// first failure, because leaving the remaining file handles open would leak
// them for the lifetime of the process.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	var firstErr error
	for _, s := range l.segments {
		if err := s.close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	l.segments = nil
	return firstErr
}
