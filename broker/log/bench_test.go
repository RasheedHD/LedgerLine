package log

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// A usage event is a few hundred bytes of JSON. Benchmarking with a realistic
// size matters: at 8 bytes the framing overhead dominates, and at 1 MiB the
// syscall cost disappears into the memory copy. Neither would tell us anything
// about this workload.
var benchPayload = bytes.Repeat([]byte("x"), 256)

// BenchmarkAppend measures the cost of each durability policy.
//
// This is the whole point of having the dial: the difference between these
// numbers is what "durable" costs, and it should be a measurement rather than
// a claim repeated from someone else's blog post.
func BenchmarkAppend(b *testing.B) {
	policies := []struct {
		name string
		opts Options
	}{
		{"SyncNever", Options{Sync: SyncNever}},
		{"SyncEveryN_100", Options{Sync: SyncEveryN, SyncEveryNRecords: 100}},
		{"SyncEveryN_1000", Options{Sync: SyncEveryN, SyncEveryNRecords: 1000}},
		{"SyncAlways", Options{Sync: SyncAlways}},
	}

	for _, p := range policies {
		b.Run(p.name, func(b *testing.B) {
			l, err := Open(b.TempDir(), p.opts)
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			defer l.Close()

			b.SetBytes(int64(len(benchPayload)))
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := l.Append(benchPayload); err != nil {
					b.Fatalf("append: %v", err)
				}
			}
		})
	}
}

// BenchmarkRead measures random reads across a log large enough to span many
// segments, which is where the index earns its keep.
//
// Random rather than sequential on purpose: sequential reads would walk
// forward from one index entry and hide the seek cost entirely.
func BenchmarkRead(b *testing.B) {
	const records = 50000

	l, err := Open(b.TempDir(), Options{MaxSegmentBytes: 1 << 20})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	defer l.Close()

	for i := 0; i < records; i++ {
		if _, err := l.Append(benchPayload); err != nil {
			b.Fatalf("append: %v", err)
		}
	}

	rng := rand.New(rand.NewSource(1))
	b.SetBytes(int64(len(benchPayload)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if _, err := l.Read(uint64(rng.Intn(records))); err != nil {
			b.Fatalf("read: %v", err)
		}
	}
}

// BenchmarkOpen measures startup on a log that already holds data.
//
// This is the number the sparse index was built to improve. Recovery resumes
// from the last index entry rather than the segment header, so the scan is
// bounded by the index interval instead of the segment size.
func BenchmarkOpen(b *testing.B) {
	for _, records := range []int{1000, 20000} {
		b.Run(fmt.Sprintf("records_%d", records), func(b *testing.B) {
			dir := b.TempDir()

			l, err := Open(dir, Options{MaxSegmentBytes: 4 << 20})
			if err != nil {
				b.Fatalf("open: %v", err)
			}
			for i := 0; i < records; i++ {
				if _, err := l.Append(benchPayload); err != nil {
					b.Fatalf("append: %v", err)
				}
			}
			l.Close()

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				reopened, err := Open(dir, Options{MaxSegmentBytes: 4 << 20})
				if err != nil {
					b.Fatalf("reopen: %v", err)
				}
				reopened.Close()
			}
		})
	}
}
