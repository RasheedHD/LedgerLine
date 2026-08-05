package log

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
)

// The index maps offsets to file positions so a read can seek near its target
// instead of scanning a segment from the beginning.
//
// It is SPARSE: one entry per indexIntervalBytes of segment data, not one per
// record. A dense index costs memory proportional to record count -- 8 bytes
// each, roughly 800 MB at a hundred million records -- which is precisely the
// cost that stops a log from holding more than fits in RAM. A sparse index
// costs memory proportional to segment *bytes*, so a 64 MiB segment indexed
// every 4 KiB needs 16k entries regardless of whether it holds a thousand
// large records or a million small ones.
//
// The tradeoff bought with that saving is a short forward scan on every read,
// bounded by the interval.
//
// Entry layout, 8 bytes each:
//
//	+---------------+---------------+
//	| relOffset     | position      |
//	| 4 bytes       | 4 bytes       |
//	+---------------+---------------+
//
// Both are relative to the segment: relOffset counts from the segment's base
// offset and position from the start of its file. Relative rather than
// absolute is what keeps them 4 bytes instead of 8, halving the index. It caps
// a segment at 4 GiB and 4 billion records, comfortably above the 64 MiB roll
// threshold.
const (
	indexEntrySize      = 8
	indexIntervalBytes  = 4 << 10
	indexFileSuffix     = ".index"
	maxIndexableSegment = 1 << 32
)

type indexEntry struct {
	relOffset uint32
	position  uint32
}

type index struct {
	file *os.File

	// Held in memory for lookups and appended to the file as they are created.
	// Small enough to keep resident by design -- that is the whole point of
	// being sparse.
	entries []indexEntry
}

func indexName(baseOffset uint64) string {
	return fmt.Sprintf("%020d%s", baseOffset, indexFileSuffix)
}

// openIndex opens or creates the index file and loads its entries.
//
// A damaged or partially written index is not an error: it is discarded and
// rebuilt from the segment, because the segment is the only authority on what
// the log contains. The index is a derived artifact and must never be able to
// make the log unreadable.
func openIndex(path string) (*index, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open index: %w", err)
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("stat index: %w", err)
	}

	ix := &index{file: file}

	// A trailing partial entry is what a crash mid-write leaves. Round down
	// and ignore it rather than reading a half-written position.
	count := info.Size() / indexEntrySize
	if count == 0 {
		return ix, nil
	}

	raw := make([]byte, count*indexEntrySize)
	if _, err := io.ReadFull(file, raw); err != nil {
		// Unreadable index, but the segment is intact. Start empty and let it
		// be rebuilt.
		return &index{file: file}, nil
	}

	ix.entries = make([]indexEntry, 0, count)
	for i := int64(0); i < count; i++ {
		e := raw[i*indexEntrySize : (i+1)*indexEntrySize]
		ix.entries = append(ix.entries, indexEntry{
			relOffset: binary.BigEndian.Uint32(e[0:4]),
			position:  binary.BigEndian.Uint32(e[4:8]),
		})
	}
	return ix, nil
}

// append records that the record at relOffset begins at position.
func (ix *index) append(relOffset uint32, position int64) error {
	if position >= maxIndexableSegment {
		return fmt.Errorf("index: position %d exceeds addressable segment size", position)
	}

	var buf [indexEntrySize]byte
	binary.BigEndian.PutUint32(buf[0:4], relOffset)
	binary.BigEndian.PutUint32(buf[4:8], uint32(position))

	if _, err := ix.file.WriteAt(buf[:], int64(len(ix.entries))*indexEntrySize); err != nil {
		return fmt.Errorf("write index entry: %w", err)
	}

	ix.entries = append(ix.entries, indexEntry{relOffset: relOffset, position: uint32(position)})
	return nil
}

// lookup returns the closest indexed entry at or before relOffset, and whether
// one exists. A read starts scanning from there.
func (ix *index) lookup(relOffset uint32) (indexEntry, bool) {
	// The first entry strictly greater than the target; the one before it is
	// the closest at or before.
	i := sort.Search(len(ix.entries), func(i int) bool {
		return ix.entries[i].relOffset > relOffset
	})
	if i == 0 {
		return indexEntry{}, false
	}
	return ix.entries[i-1], true
}

// last returns the final entry, the nearest known-good starting point for a
// recovery scan.
func (ix *index) last() (indexEntry, bool) {
	if len(ix.entries) == 0 {
		return indexEntry{}, false
	}
	return ix.entries[len(ix.entries)-1], true
}

// truncateFrom drops every entry pointing at or beyond position, and shrinks
// the file to match.
//
// Called after a segment's damaged tail is truncated. An index entry pointing
// into bytes that no longer exist would send a reader past the end of the
// file, so the two must be cut back together.
func (ix *index) truncateFrom(position int64) error {
	kept := 0
	for _, e := range ix.entries {
		if int64(e.position) >= position {
			break
		}
		kept++
	}
	ix.entries = ix.entries[:kept]

	if err := ix.file.Truncate(int64(kept) * indexEntrySize); err != nil {
		return fmt.Errorf("truncate index: %w", err)
	}
	return nil
}

// reset empties the index completely, for when it has to be rebuilt from the
// segment.
func (ix *index) reset() error {
	ix.entries = ix.entries[:0]
	if err := ix.file.Truncate(0); err != nil {
		return fmt.Errorf("reset index: %w", err)
	}
	return nil
}

func (ix *index) sync() error  { return ix.file.Sync() }
func (ix *index) close() error { return ix.file.Close() }
