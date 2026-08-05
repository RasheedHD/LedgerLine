package log

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Segment file layout:
//
//	+---------+-----------+-------------+-----------------+
//	| magic   | version   | baseOffset  | records...      |
//	| 4 bytes | 4 bytes   | 8 bytes     |                 |
//	+---------+-----------+-------------+-----------------+
//
// The magic number is checked on open so that pointing the log at the wrong
// directory fails immediately rather than being interpreted as a corrupt
// segment and truncated to nothing. The version field exists so a future
// format change is a readable error rather than silent misparsing.
const (
	segmentMagic   = 0x4C474C4E // "LGLN"
	segmentVersion = 1

	magicSize         = 4
	versionSize       = 4
	baseOffsetSize    = 8
	segmentHeaderSize = magicSize + versionSize + baseOffsetSize
)

var (
	// ErrBadMagic means the file is not one of ours.
	ErrBadMagic = errors.New("log: not a segment file")

	// ErrBadVersion means the file was written by an incompatible build.
	ErrBadVersion = errors.New("log: unsupported segment version")

	// ErrOffsetOutOfRange is returned by Read for an offset the log does not
	// hold.
	ErrOffsetOutOfRange = errors.New("log: offset out of range")
)

// segment is one file of the log.
//
// Not safe for concurrent use on its own. Log owns the locking, because
// rolling from one segment to the next has to be atomic with respect to
// appends and that can only be coordinated a level up.
type segment struct {
	file       *os.File
	baseOffset uint64
	nextOffset uint64

	// Byte offset of the end of the last complete record, which is where the
	// next one goes.
	size int64

	// positions[i] is the file position of the record at offset
	// baseOffset+i.
	//
	// This is a DENSE in-memory index, and it is deliberately the simple
	// version: 8 bytes of memory per record, which at a hundred million
	// records is 800 MB. That cost is exactly the reason Kafka keeps a sparse
	// index on disk instead, and replacing this is the next step in Phase 2.
	// It is correct, just not scalable.
	positions []int64
}

func segmentName(baseOffset uint64) string {
	// Zero-padded so lexical filename order matches numeric offset order,
	// which is what makes listing a directory enough to recover segment
	// sequence. 20 digits holds any uint64.
	return fmt.Sprintf("%020d.log", baseOffset)
}

// createSegment starts a new segment file at baseOffset.
func createSegment(dir string, baseOffset uint64) (*segment, error) {
	path := filepath.Join(dir, segmentName(baseOffset))

	// O_EXCL so that silently reopening and appending to an existing segment
	// is impossible -- that would corrupt the offset sequence in a way no
	// checksum would catch, because every individual record would be valid.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create segment: %w", err)
	}

	header := make([]byte, segmentHeaderSize)
	binary.BigEndian.PutUint32(header[0:magicSize], segmentMagic)
	binary.BigEndian.PutUint32(header[magicSize:magicSize+versionSize], segmentVersion)
	binary.BigEndian.PutUint64(header[magicSize+versionSize:], baseOffset)

	if _, err := file.WriteAt(header, 0); err != nil {
		file.Close()
		return nil, fmt.Errorf("write segment header: %w", err)
	}

	return &segment{
		file:       file,
		baseOffset: baseOffset,
		nextOffset: baseOffset,
		size:       segmentHeaderSize,
	}, nil
}

// openSegment opens an existing segment and recovers it.
//
// Recovery is not a separate step a caller can forget: a segment that has not
// been scanned has an unknown end position, and appending to it would write
// over or after garbage.
func openSegment(path string) (*segment, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open segment: %w", err)
	}

	header := make([]byte, segmentHeaderSize)
	if _, err := io.ReadFull(file, header); err != nil {
		file.Close()
		return nil, fmt.Errorf("read segment header: %w", err)
	}

	if magic := binary.BigEndian.Uint32(header[0:magicSize]); magic != segmentMagic {
		file.Close()
		return nil, fmt.Errorf("%w: magic %08x", ErrBadMagic, magic)
	}
	if version := binary.BigEndian.Uint32(header[magicSize : magicSize+versionSize]); version != segmentVersion {
		file.Close()
		return nil, fmt.Errorf("%w: version %d, this build writes %d", ErrBadVersion, version, segmentVersion)
	}

	s := &segment{
		file:       file,
		baseOffset: binary.BigEndian.Uint64(header[magicSize+versionSize:]),
		size:       segmentHeaderSize,
	}
	s.nextOffset = s.baseOffset

	if err := s.recover(); err != nil {
		file.Close()
		return nil, err
	}
	return s, nil
}

// recover scans forward from the header, rebuilding the position table and
// finding where the good records end.
//
// CRITICAL: this is where crash safety lives.
//
// A process killed mid-append leaves a partial record at the tail. Every
// record before it is intact -- writes are sequential, so a torn tail is the
// only shape a crash can leave. The scan stops at the first record that fails
// to read and truncates the file there, so the segment resumes from a clean
// boundary and a reader can never be handed a partial record.
//
// The assumption this makes explicit: damage is at the tail. Bit rot in the
// middle of a segment would be detected by the checksum, but truncating there
// discards every valid record after it. That is the same tradeoff Kafka
// makes, and it is a real limitation rather than an oversight.
func (s *segment) recover() error {
	if _, err := s.file.Seek(segmentHeaderSize, io.SeekStart); err != nil {
		return fmt.Errorf("seek to first record: %w", err)
	}

	// Buffered because this reads every record in the segment one small header
	// at a time, and an unbuffered syscall per read makes startup crawl.
	reader := bufio.NewReader(s.file)
	position := int64(segmentHeaderSize)

	for {
		_, n, err := readRecord(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Clean end on a record boundary: nothing to repair.
				break
			}
			if errors.Is(err, ErrTornRecord) || errors.Is(err, ErrCorruptRecord) {
				// Everything from `position` on is unreadable. Drop it.
				if truncErr := s.file.Truncate(position); truncErr != nil {
					return fmt.Errorf("truncate damaged tail at %d: %w", position, truncErr)
				}
				break
			}
			return fmt.Errorf("scan segment: %w", err)
		}

		s.positions = append(s.positions, position)
		position += int64(n)
		s.nextOffset++
	}

	s.size = position
	return nil
}

// append writes one record and returns the offset it was stored at.
func (s *segment) append(payload []byte) (uint64, error) {
	encoded, err := encodeRecord(payload)
	if err != nil {
		return 0, err
	}

	// WriteAt against the tracked size rather than O_APPEND, so the write
	// position is something this code decides and can assert on, not a
	// property of how the file happened to be opened.
	if _, err := s.file.WriteAt(encoded, s.size); err != nil {
		return 0, fmt.Errorf("write record: %w", err)
	}

	offset := s.nextOffset
	s.positions = append(s.positions, s.size)
	s.size += int64(len(encoded))
	s.nextOffset++

	return offset, nil
}

// read returns the payload stored at offset.
func (s *segment) read(offset uint64) ([]byte, error) {
	if offset < s.baseOffset || offset >= s.nextOffset {
		return nil, fmt.Errorf("%w: %d not in segment [%d,%d)", ErrOffsetOutOfRange, offset, s.baseOffset, s.nextOffset)
	}

	position := s.positions[offset-s.baseOffset]

	// A SectionReader over the file rather than a Seek, because Seek mutates
	// shared file state and a concurrent reader would race with it. ReadAt,
	// which SectionReader uses, takes the position as an argument instead.
	section := io.NewSectionReader(s.file, position, s.size-position)

	payload, _, err := readRecord(section)
	if err != nil {
		return nil, fmt.Errorf("read offset %d: %w", offset, err)
	}
	return payload, nil
}

// sync forces this segment's writes to durable storage.
//
// Not called by append. See ADR-0006 for the durability policy and what that
// choice does and does not protect against.
func (s *segment) sync() error {
	return s.file.Sync()
}

func (s *segment) close() error {
	return s.file.Close()
}
