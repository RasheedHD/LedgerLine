package log

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTestLog(t *testing.T, dir string, maxSegmentBytes int64) *Log {
	t.Helper()
	return openTestLogWith(t, dir, Options{MaxSegmentBytes: maxSegmentBytes})
}

func openTestLogWith(t *testing.T, dir string, opts Options) *Log {
	t.Helper()
	l, err := Open(dir, opts)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func appendN(t *testing.T, l *Log, n int) [][]byte {
	t.Helper()
	payloads := make([][]byte, n)
	for i := 0; i < n; i++ {
		payloads[i] = []byte(fmt.Sprintf("record-%04d", i))
		offset, err := l.Append(payloads[i])
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if offset != uint64(i) {
			t.Fatalf("append %d returned offset %d -- offsets must be dense and start at zero", i, offset)
		}
	}
	return payloads
}

// Offsets are assigned densely from zero and every record reads back byte for
// byte.
func TestAppendAndRead(t *testing.T) {
	l := openTestLog(t, t.TempDir(), 0)

	payloads := appendN(t, l, 100)

	if got := l.NextOffset(); got != 100 {
		t.Errorf("NextOffset = %d, want 100", got)
	}
	for i, want := range payloads {
		got, err := l.Read(uint64(i))
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("read %d = %q, want %q", i, got, want)
		}
	}
}

func TestReadOutOfRange(t *testing.T) {
	l := openTestLog(t, t.TempDir(), 0)
	appendN(t, l, 3)

	for _, offset := range []uint64{3, 4, 1 << 40} {
		if _, err := l.Read(offset); !errors.Is(err, ErrOffsetOutOfRange) {
			t.Errorf("read(%d) error = %v, want ErrOffsetOutOfRange", offset, err)
		}
	}
}

// Records land in multiple files once the size threshold is passed, and reads
// still resolve across the boundary.
func TestSegmentRolling(t *testing.T) {
	dir := t.TempDir()

	// Small enough that a handful of records forces several rolls.
	l := openTestLog(t, dir, 200)
	payloads := appendN(t, l, 60)

	entries, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("segment count = %d, want more than 1 -- rolling never happened", len(entries))
	}

	// Reading across segment boundaries is the point of rolling; a record in
	// the third file must be as reachable as one in the first.
	for i, want := range payloads {
		got, err := l.Read(uint64(i))
		if err != nil {
			t.Fatalf("read %d across %d segments: %v", i, len(entries), err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("read %d = %q, want %q", i, got, want)
		}
	}

	// No segment may exceed its bound, or the reason for rolling is defeated.
	for _, path := range entries {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Size() > 200+int64(recordHeaderSize+len("record-0000")) {
			t.Errorf("segment %s is %d bytes, well past the 200 byte threshold", filepath.Base(path), info.Size())
		}
	}
}

// Closing and reopening preserves every record and continues the offset
// sequence rather than restarting it.
func TestReopenPreservesLog(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir, Options{MaxSegmentBytes: 300})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	payloads := appendN(t, first, 40)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second := openTestLog(t, dir, 300)

	if got := second.NextOffset(); got != 40 {
		t.Fatalf("NextOffset after reopen = %d, want 40 -- reusing offsets would overwrite history", got)
	}
	for i, want := range payloads {
		got, err := second.Read(uint64(i))
		if err != nil {
			t.Fatalf("read %d after reopen: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("read %d after reopen = %q, want %q", i, got, want)
		}
	}

	// The reopened log must be writable, at the correct next offset.
	offset, err := second.Append([]byte("after-reopen"))
	if err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if offset != 40 {
		t.Errorf("append after reopen returned offset %d, want 40", offset)
	}
}

// CRITICAL TEST: the crash case.
//
// A process killed mid-append leaves a partial record at the tail of the
// active segment. Simulated here by truncating the file, which produces
// exactly the byte pattern a torn write leaves behind.
//
// What must hold afterwards: every record that was fully written is still
// readable, the partial one is gone rather than being returned as a short
// payload, and the log accepts writes again at the right offset.
func TestRecoversFromTornTail(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	payloads := appendN(t, first, 20)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Cut the last record in half.
	path := filepath.Join(dir, segmentName(0))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	tornAt := info.Size() - int64(len(payloads[0])/2)
	if err := os.Truncate(path, tornAt); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	second := openTestLog(t, dir, 0)

	if got := second.NextOffset(); got != 19 {
		t.Fatalf("NextOffset after recovery = %d, want 19 -- the torn record must not be counted", got)
	}

	// The 19 complete records are untouched.
	for i := 0; i < 19; i++ {
		got, err := second.Read(uint64(i))
		if err != nil {
			t.Fatalf("read %d after recovery: %v", i, err)
		}
		if !bytes.Equal(got, payloads[i]) {
			t.Errorf("read %d after recovery = %q, want %q", i, got, payloads[i])
		}
	}

	// The torn one is not readable as a truncated payload.
	if _, err := second.Read(19); !errors.Is(err, ErrOffsetOutOfRange) {
		t.Errorf("reading the torn offset returned %v, want ErrOffsetOutOfRange", err)
	}

	// The damaged tail must actually be removed from the file, not merely
	// skipped in memory -- otherwise the next append writes after garbage and
	// the corruption becomes permanent.
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat after recovery: %v", err)
	} else if info.Size() >= tornAt {
		t.Errorf("segment is still %d bytes, expected truncation below %d", info.Size(), tornAt)
	}

	// And the log is writable again, reusing the offset the torn record never
	// successfully claimed.
	offset, err := second.Append([]byte("after-recovery"))
	if err != nil {
		t.Fatalf("append after recovery: %v", err)
	}
	if offset != 19 {
		t.Errorf("append after recovery returned offset %d, want 19", offset)
	}
	got, err := second.Read(19)
	if err != nil || !bytes.Equal(got, []byte("after-recovery")) {
		t.Errorf("read back after recovery = %q, %v", got, err)
	}
}

// Bit rot in the middle of the active segment stops the scan there. The
// records before it survive; the ones after are discarded. That is the
// tradeoff recorded in segment.recover -- damage is assumed to be at the tail,
// and this test pins down what actually happens when it is not.
func TestCorruptRecordTruncatesFromThatPoint(t *testing.T) {
	dir := t.TempDir()

	first, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	appendN(t, first, 10)
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Flip a bit inside the payload of the fifth record.
	path := filepath.Join(dir, segmentName(0))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read segment: %v", err)
	}
	recordLen := recordHeaderSize + len("record-0000")
	corruptAt := segmentHeaderSize + 4*recordLen + recordHeaderSize + 2
	raw[corruptAt] ^= 0x01
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("write segment: %v", err)
	}

	second := openTestLog(t, dir, 0)

	if got := second.NextOffset(); got != 4 {
		t.Fatalf("NextOffset = %d, want 4 -- the scan must stop at the damaged record", got)
	}
	for i := 0; i < 4; i++ {
		if _, err := second.Read(uint64(i)); err != nil {
			t.Errorf("read %d: %v -- records before the damage must survive", i, err)
		}
	}
}

// Concurrent writers must each receive a distinct offset, and concurrent
// readers must never observe a partially appended record.
//
// This is what the RWMutex in Log is for. Without it two appends can compute
// the same write position and the second silently overwrites the first --
// losing a record with no error anywhere, which is invariant I3 broken in the
// quietest possible way.
//
// Best run under -race, which cannot execute on this machine (see D21). Even
// without it, duplicate or missing offsets show up here.
func TestConcurrentAppendsGetDistinctOffsets(t *testing.T) {
	l := openTestLog(t, t.TempDir(), 0)

	const writers = 32
	const perWriter = 25
	const total = writers * perWriter

	offsets := make(chan uint64, total)
	done := make(chan struct{})

	// A reader running throughout, so appends and reads genuinely overlap.
	go func() {
		defer close(done)
		for {
			next := l.NextOffset()
			if next >= total {
				return
			}
			if next > 0 {
				// Any successful read must return a complete record.
				if payload, err := l.Read(next - 1); err == nil && len(payload) == 0 {
					t.Errorf("read a zero-length payload for offset %d", next-1)
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				offset, err := l.Append([]byte(fmt.Sprintf("w%02d-r%02d", w, i)))
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
				offsets <- offset
			}
		}(w)
	}
	wg.Wait()
	close(offsets)
	<-done

	seen := make(map[uint64]bool, total)
	for offset := range offsets {
		if seen[offset] {
			t.Fatalf("offset %d was handed out twice -- one record silently overwrote another", offset)
		}
		seen[offset] = true
	}
	if len(seen) != total {
		t.Fatalf("got %d distinct offsets, want %d", len(seen), total)
	}
	if got := l.NextOffset(); got != total {
		t.Errorf("NextOffset = %d, want %d", got, total)
	}

	// Every record must be readable, which also proves no write landed on top
	// of another.
	for offset := uint64(0); offset < total; offset++ {
		if _, err := l.Read(offset); err != nil {
			t.Fatalf("read %d: %v", offset, err)
		}
	}
}

func TestRejectsForeignFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, segmentName(0))
	if err := os.WriteFile(path, []byte("this is not a segment file at all"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Open(dir, Options{}); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("error = %v, want ErrBadMagic -- an unrecognised file must not be truncated to nothing", err)
	}
}
