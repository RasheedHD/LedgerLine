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

// Options configures a Log.
type Options struct {
	// MaxSegmentBytes is the size at which the active segment is closed and a
	// new one started. Zero means DefaultMaxSegmentBytes.
	MaxSegmentBytes int64
}

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

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}

	baseOffsets, err := listSegmentOffsets(dir)
	if err != nil {
		return nil, err
	}

	l := &Log{dir: dir, opts: opts}

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
// The returned offset is durable only to the extent Sync has been called. See
// ADR-0006: on return the record has been handed to the operating system, which
// survives this process dying but not the machine losing power.
func (l *Log) Append(payload []byte) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	active := l.segments[len(l.segments)-1]

	// Roll before writing rather than after, so a record is never split across
	// segments and a segment never exceeds its bound. The record-count check
	// keeps a single record larger than the threshold from producing an
	// endless sequence of empty segments -- it goes into a fresh segment on
	// its own and overshoots, which is the lesser problem.
	recordSize := int64(recordHeaderSize + len(payload))
	if len(active.positions) > 0 && active.size+recordSize > l.opts.MaxSegmentBytes {
		next, err := createSegment(l.dir, active.nextOffset)
		if err != nil {
			return 0, err
		}
		l.segments = append(l.segments, next)
		active = next
	}

	return active.append(payload)
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
