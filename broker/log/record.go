// Package log implements an append-only, crash-safe record log with
// offset-addressed reads.
//
// Note the package name shadows the standard library's log package. Callers
// needing both must alias one of them. The name follows the directory, which
// is what the project layout calls for.
//
// The design follows Kafka's: records are framed with a length and a checksum,
// grouped into segment files that roll at a size threshold, and addressed by a
// monotonically increasing offset.
package log

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// Record framing on disk:
//
//	+----------+----------+------------------+
//	| length   | crc32c   | payload          |
//	| 4 bytes  | 4 bytes  | `length` bytes   |
//	+----------+----------+------------------+
//
// Both header fields are big-endian, which is arbitrary but must never change:
// it is the difference between reading a log written by an older build and
// reading garbage.
const (
	lengthSize       = 4
	crcSize          = 4
	recordHeaderSize = lengthSize + crcSize

	// An upper bound on a single record, checked before any allocation. A
	// corrupted length field is otherwise an instruction to allocate an
	// arbitrary amount of memory, which turns disk corruption into an
	// out-of-memory crash.
	MaxRecordSize = 8 << 20
)

// Castagnoli rather than the IEEE polynomial: it has hardware support on every
// CPU this will realistically run on, and it is what Kafka, RocksDB, and
// LevelDB use for the same job.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

var (
	// ErrTornRecord means the file ends in the middle of a record. This is the
	// expected result of a process dying mid-append, not a sign of a damaged
	// disk, and recovery handles it by truncating.
	ErrTornRecord = errors.New("log: torn record at end of segment")

	// ErrCorruptRecord means a record was read in full but its checksum does
	// not match, or its length is implausible. Unlike a torn record this
	// indicates the bytes themselves are wrong.
	ErrCorruptRecord = errors.New("log: corrupt record")

	// ErrRecordTooLarge is returned by Append, before anything is written.
	ErrRecordTooLarge = errors.New("log: record exceeds maximum size")
)

// encodeRecord returns the on-disk representation of one payload.
func encodeRecord(payload []byte) ([]byte, error) {
	if len(payload) > MaxRecordSize {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrRecordTooLarge, len(payload), MaxRecordSize)
	}

	buf := make([]byte, recordHeaderSize+len(payload))
	binary.BigEndian.PutUint32(buf[0:lengthSize], uint32(len(payload)))

	// CRITICAL: the checksum covers the length field as well as the payload.
	//
	// Checksumming only the payload leaves the length unprotected, and the
	// length is what tells the reader how many bytes to consume. A flipped bit
	// there does not corrupt one record -- it desynchronises the reader from
	// the record stream, so every subsequent record in the segment is read
	// from the wrong position. Covering the length makes that detectable at
	// the record where it happened.
	crc := crc32.New(crcTable)
	crc.Write(buf[0:lengthSize])
	crc.Write(payload)
	binary.BigEndian.PutUint32(buf[lengthSize:recordHeaderSize], crc.Sum32())

	copy(buf[recordHeaderSize:], payload)
	return buf, nil
}

// readRecord reads one record from r, returning the payload and the total
// number of bytes consumed including the header.
//
// The byte count is what recovery uses to track the last known-good position,
// so it is returned even on failure paths where it is zero.
func readRecord(r io.Reader) ([]byte, int, error) {
	header := make([]byte, recordHeaderSize)

	// io.ReadFull distinguishes the two endings that matter: io.EOF means the
	// file ended cleanly on a record boundary, io.ErrUnexpectedEOF means it
	// ended partway through one.
	if _, err := io.ReadFull(r, header); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, 0, ErrTornRecord
		}
		return nil, 0, err
	}

	length := binary.BigEndian.Uint32(header[0:lengthSize])
	if length > MaxRecordSize {
		return nil, 0, fmt.Errorf("%w: length %d exceeds maximum %d", ErrCorruptRecord, length, MaxRecordSize)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, 0, ErrTornRecord
		}
		return nil, 0, err
	}

	want := binary.BigEndian.Uint32(header[lengthSize:recordHeaderSize])
	crc := crc32.New(crcTable)
	crc.Write(header[0:lengthSize])
	crc.Write(payload)
	if got := crc.Sum32(); got != want {
		return nil, 0, fmt.Errorf("%w: checksum %08x, want %08x", ErrCorruptRecord, got, want)
	}

	return payload, recordHeaderSize + int(length), nil
}
