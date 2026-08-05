package log

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// countIndexEntries reports how many entries the on-disk index holds.
func countIndexEntries(t *testing.T, dir string, baseOffset uint64) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(dir, indexName(baseOffset)))
	if err != nil {
		t.Fatalf("stat index: %v", err)
	}
	return info.Size() / indexEntrySize
}

// The index must be genuinely sparse. A dense one would work identically for
// every other test in this file while costing memory proportional to record
// count -- which is the whole thing the sparse design exists to avoid, so it
// needs asserting rather than assuming.
func TestIndexIsSparse(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir, 0)

	// Small records, so a dense index would have one entry each.
	const records = 4000
	payload := bytes.Repeat([]byte("y"), 64)
	for i := 0; i < records; i++ {
		if _, err := l.Append(payload); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	entries := countIndexEntries(t, dir, 0)

	// Records occupy roughly 72 bytes each, so at a 4 KiB interval there
	// should be about one entry per 57 records.
	if entries >= records/10 {
		t.Errorf("index holds %d entries for %d records -- that is not sparse", entries, records)
	}
	if entries == 0 {
		t.Error("index holds no entries at all; every read will scan from the segment header")
	}
	t.Logf("%d entries for %d records (%.1f records per entry)", entries, records, float64(records)/float64(entries))
}

// Every offset must be readable regardless of where it falls relative to an
// index entry -- exactly on one, just after one, and just before the next.
// The forward scan between entries is where an off-by-one would hide.
func TestReadsResolveBetweenIndexEntries(t *testing.T) {
	dir := t.TempDir()
	l := openTestLog(t, dir, 0)

	const records = 3000
	payloads := make([][]byte, records)
	for i := 0; i < records; i++ {
		payloads[i] = []byte(fmt.Sprintf("record-%06d-%s", i, bytes.Repeat([]byte("z"), i%40)))
		if _, err := l.Append(payloads[i]); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	for i := 0; i < records; i++ {
		got, err := l.Read(uint64(i))
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(got, payloads[i]) {
			t.Fatalf("read %d returned the wrong record -- the forward scan landed in the wrong place", i)
		}
	}
}

// The index is a derived artifact. Losing it must cost startup time, not data:
// the segment is the only authority on what the log contains.
func TestLogSurvivesDeletedIndex(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	payloads := appendN(t, first, 500)
	first.Close()

	if err := os.Remove(filepath.Join(dir, indexName(0))); err != nil {
		t.Fatalf("remove index: %v", err)
	}

	second := openTestLog(t, dir, 0)

	if got := second.NextOffset(); got != 500 {
		t.Fatalf("NextOffset = %d, want 500 -- losing the index lost records", got)
	}
	for i, want := range payloads {
		got, err := second.Read(uint64(i))
		if err != nil {
			t.Fatalf("read %d after index loss: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("read %d = %q, want %q", i, got, want)
		}
	}

	// It must also have been rebuilt, not merely worked around.
	if entries := countIndexEntries(t, dir, 0); entries == 0 {
		t.Error("index was not rebuilt after being deleted")
	}
}

// A crash can leave a partially written index entry. It must be ignored rather
// than read as a valid position, which would send a reader to an arbitrary
// place in the segment.
func TestTruncatedIndexEntryIsIgnored(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	payloads := appendN(t, first, 500)
	first.Close()

	// Cut the index mid-entry.
	path := filepath.Join(dir, indexName(0))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat index: %v", err)
	}
	if err := os.Truncate(path, info.Size()-indexEntrySize/2); err != nil {
		t.Fatalf("truncate index: %v", err)
	}

	second := openTestLog(t, dir, 0)

	if got := second.NextOffset(); got != 500 {
		t.Fatalf("NextOffset = %d, want 500", got)
	}
	for i, want := range payloads {
		got, err := second.Read(uint64(i))
		if err != nil {
			t.Fatalf("read %d with a torn index: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("read %d = %q, want %q", i, got, want)
		}
	}
}

// An index entry pointing past the end of its segment means the index outlived
// data it described -- possible because they are separate files with no write
// ordering between them. It must be discarded, not followed.
func TestIndexPointingPastSegmentEndIsDiscarded(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	payloads := appendN(t, first, 500)
	first.Close()

	// Lose the tail of the segment while leaving the index describing it.
	segPath := filepath.Join(dir, segmentName(0))
	info, err := os.Stat(segPath)
	if err != nil {
		t.Fatalf("stat segment: %v", err)
	}
	if err := os.Truncate(segPath, info.Size()/2); err != nil {
		t.Fatalf("truncate segment: %v", err)
	}

	second := openTestLog(t, dir, 0)

	total := second.NextOffset()
	if total == 0 || total >= 500 {
		t.Fatalf("NextOffset = %d, want a partial recovery below 500", total)
	}

	// Whatever survived must be genuinely readable, not a position inherited
	// from a stale index entry.
	for i := uint64(0); i < total; i++ {
		got, err := second.Read(i)
		if err != nil {
			t.Fatalf("read %d after partial recovery: %v", i, err)
		}
		if !bytes.Equal(got, payloads[i]) {
			t.Fatalf("read %d returned the wrong record after recovery", i)
		}
	}
}
