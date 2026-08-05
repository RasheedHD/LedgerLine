package log

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Environment variables used to turn this test binary into the child process
// that gets killed. Re-executing the test binary is the standard Go way to get
// a real process to kill without building a separate helper command.
const (
	crashChildEnv = "LEDGERLINE_CRASH_CHILD"
	crashDirEnv   = "LEDGERLINE_CRASH_DIR"
)

// TestCrashDuringAppend kills a process in the middle of writing to the log and
// asserts the log is still usable afterwards.
//
// This is the real version of the crash case. TestRecoversFromTornTail
// simulates damage by truncating a file, which is useful because it is
// deterministic, but it only proves recovery handles the byte pattern we
// *expect* a crash to leave. This one produces whatever a genuine SIGKILL
// actually leaves, which is the thing that has to be survivable.
//
// Note precisely what it does and does not prove. Killing a process does not
// lose data the process already handed to the operating system -- the page
// cache belongs to the kernel and outlives the process. So this establishes
// that a crashed writer never leaves a log that cannot be reopened, and never
// leaves a partial record visible to a reader. It says nothing about power
// loss, which is what fsync addresses and what ADR-0006 covers.
//
// MEASURED RESULT, worth knowing before trusting this test too far: across
// repeated runs the tail is never actually torn. Each record is written with a
// single WriteAt, the kernel completes that syscall before delivering the
// kill, and Go retries short writes internally. A process cannot be
// interrupted partway through its own write.
//
// So a torn record is not something SIGKILL can produce at all -- it needs the
// machine to stop between the kernel accepting a write and the disk committing
// it. That is why TestRecoversFromTornTail constructs the damage directly:
// it is the only way to exercise the repair path, and the two tests cover
// genuinely different failures rather than duplicating each other.
//
// The `torn` value below is reported rather than asserted, because requiring a
// tear would make this test fail for the right reason on a correct system.
func TestCrashDuringAppend(t *testing.T) {
	if os.Getenv(crashChildEnv) == "1" {
		runCrashChild()
		return
	}

	dir := t.TempDir()

	// -test.run pins the child to this one test so it does not re-run the
	// whole suite before reaching the append loop.
	child := exec.Command(os.Args[0], "-test.run=^TestCrashDuringAppend$")
	child.Env = append(os.Environ(), crashChildEnv+"=1", crashDirEnv+"="+dir)

	if err := child.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}

	// Long enough to get well into the append loop, short enough to keep the
	// suite quick. The exact moment of the kill is deliberately unsynchronised
	// -- landing mid-write is the entire point, and a coordinated kill would
	// only ever test the boundary we chose.
	time.Sleep(400 * time.Millisecond)

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = child.Wait() // A killed process always reports an error; it carries no information here.

	// Before recovering, establish whether the kill actually landed mid-record.
	// Without this the test could pass trivially -- if every write happened to
	// complete, recovery would have nothing to repair and "all records intact"
	// would prove only that appending works.
	torn := lastSegmentEndsMidRecord(t, dir)

	l, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("log could not be reopened after a crash: %v", err)
	}
	defer l.Close()

	total := l.NextOffset()
	if total == 0 {
		t.Fatal("child wrote nothing before being killed; the test proved nothing -- increase the sleep")
	}

	// Every offset the log claims to hold must be readable and intact. A torn
	// record surviving recovery would show up here as either a read error or a
	// payload that fails its own content check.
	for offset := uint64(0); offset < total; offset++ {
		payload, err := l.Read(offset)
		if err != nil {
			t.Fatalf("offset %d unreadable after crash recovery: %v", offset, err)
		}
		if !bytes.Equal(payload, crashPayload(offset)) {
			t.Fatalf("offset %d has the wrong payload after crash recovery -- a partial record survived", offset)
		}
	}

	// Reading one past the end must fail cleanly rather than returning a
	// half-written record.
	if _, err := l.Read(total); err == nil {
		t.Error("reading past the end succeeded; a partially written record is visible")
	}

	// The recovered log must still be writable, continuing the sequence.
	offset, err := l.Append([]byte("post-crash"))
	if err != nil {
		t.Fatalf("append after crash recovery: %v", err)
	}
	if offset != total {
		t.Errorf("append after recovery used offset %d, want %d", offset, total)
	}

	t.Logf("child wrote %d records before being killed; torn tail present: %v; all recovered intact", total, torn)
}

// lastSegmentEndsMidRecord reports whether the newest segment file stops
// partway through a record, which is the signature of a write interrupted by
// the kill.
//
// Every record the child writes is the same size, so a segment that ended
// cleanly is exactly a header plus a whole number of records.
func lastSegmentEndsMidRecord(t *testing.T, dir string) bool {
	t.Helper()

	offsets, err := listSegmentOffsets(dir)
	if err != nil {
		t.Fatalf("list segments: %v", err)
	}
	if len(offsets) == 0 {
		t.Fatal("no segments were created")
	}

	path := filepath.Join(dir, segmentName(offsets[len(offsets)-1]))
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active segment: %v", err)
	}

	recordSize := int64(recordHeaderSize + len(crashPayload(0)))
	return (info.Size()-segmentHeaderSize)%recordSize != 0
}

// crashPayload builds a deterministic payload for an offset, so the parent can
// verify content without knowing how far the child got.
//
// Deliberately large: a small record is likely to reach the disk in a single
// uninterruptible write, which would make a torn tail almost impossible to
// produce and the test far weaker than it looks.
func crashPayload(offset uint64) []byte {
	seed := byte(offset % 251)
	return bytes.Repeat([]byte{seed}, 64<<10)
}

// runCrashChild appends until it is killed. It must never exit on its own --
// a clean exit would leave a cleanly closed log and test nothing.
func runCrashChild() {
	dir := os.Getenv(crashDirEnv)

	l, err := Open(dir, Options{})
	if err != nil {
		os.Exit(2)
	}

	for offset := uint64(0); ; offset++ {
		if _, err := l.Append(crashPayload(offset)); err != nil {
			os.Exit(3)
		}
	}
}
