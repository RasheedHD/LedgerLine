package log

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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

// segment is one file of the log, paired with its sparse index.
//
// Not safe for concurrent use on its own. Log owns the locking, because
// rolling from one segment to the next has to be atomic with respect to
// appends and that can only be coordinated a level up.
type segment struct {
	file       *os.File
	index      *index
	baseOffset uint64
	nextOffset uint64

	// Byte offset of the end of the last complete record, which is where the
	// next one goes.
	size int64

	// Position of the most recent index entry, used to decide when the next
	// one is due.
	lastIndexed int64
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

	ix, err := openIndex(filepath.Join(dir, indexName(baseOffset)))
	if err != nil {
		file.Close()
		return nil, err
	}

	return &segment{
		file:        file,
		index:       ix,
		baseOffset:  baseOffset,
		nextOffset:  baseOffset,
		size:        segmentHeaderSize,
		lastIndexed: segmentHeaderSize,
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

	baseOffset := binary.BigEndian.Uint64(header[magicSize+versionSize:])

	indexPath := strings.TrimSuffix(path, ".log") + indexFileSuffix
	ix, err := openIndex(indexPath)
	if err != nil {
		file.Close()
		return nil, err
	}

	s := &segment{
		file:        file,
		index:       ix,
		baseOffset:  baseOffset,
		nextOffset:  baseOffset,
		size:        segmentHeaderSize,
		lastIndexed: segmentHeaderSize,
	}

	if err := s.recover(); err != nil {
		file.Close()
		ix.close()
		return nil, err
	}
	return s, nil
}

// recover finds where the good records end, repairing a damaged tail and
// completing the index.
//
// CRITICAL: this is where crash safety lives.
//
// A process killed mid-append leaves a partial record at the tail. Every
// record before it is intact -- writes are sequential, so a torn tail is the
// only shape a crash can leave. The scan stops at the first record that fails
// to read and truncates the file there, so the segment resumes from a clean
// boundary and a reader can never be handed a partial record.
//
// The sparse index turns this from a full-segment scan into a bounded one: the
// scan resumes from the last indexed position, so at most indexIntervalBytes
// of data is re-read rather than the whole file. That is what makes opening a
// log with many large segments quick.
//
// The assumption this makes explicit: damage is at the tail. Bit rot in the
// middle of a segment would be detected by the checksum, but truncating there
// discards every valid record after it. That is the same tradeoff Kafka
// makes, and it is a real limitation rather than an oversight.
func (s *segment) recover() error {
	info, err := s.file.Stat()
	if err != nil {
		return fmt.Errorf("stat segment: %w", err)
	}
	fileSize := info.Size()

	position := int64(segmentHeaderSize)
	rel := uint32(0)

	// Resume from the last index entry when there is one that still points
	// inside the file. An entry beyond the end means the index outlived data
	// it described -- possible because the index and segment are separate
	// files with no ordering guarantee between them -- so it is discarded and
	// rebuilt rather than trusted.
	if e, ok := s.index.last(); ok {
		if int64(e.position) >= segmentHeaderSize && int64(e.position) < fileSize {
			position = int64(e.position)
			rel = e.relOffset
		} else if err := s.index.reset(); err != nil {
			return err
		}
	}
	s.lastIndexed = position

	if _, err := s.file.Seek(position, io.SeekStart); err != nil {
		return fmt.Errorf("seek to scan start: %w", err)
	}

	// Buffered because this reads records one small header at a time, and an
	// unbuffered syscall per read makes startup crawl.
	reader := bufio.NewReader(s.file)

	for {
		_, n, err := readRecord(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Clean end on a record boundary: nothing to repair.
				break
			}
			if errors.Is(err, ErrTornRecord) || errors.Is(err, ErrCorruptRecord) {
				// Everything from `position` on is unreadable. Drop it, and
				// cut the index back to match so no entry points into bytes
				// that no longer exist.
				if truncErr := s.file.Truncate(position); truncErr != nil {
					return fmt.Errorf("truncate damaged tail at %d: %w", position, truncErr)
				}
				if truncErr := s.index.truncateFrom(position); truncErr != nil {
					return truncErr
				}
				break
			}
			return fmt.Errorf("scan segment: %w", err)
		}

		if err := s.maybeIndex(rel, position); err != nil {
			return err
		}
		position += int64(n)
		rel++
	}

	s.size = position
	s.nextOffset = s.baseOffset + uint64(rel)
	return nil
}

// maybeIndex adds an index entry if one is due.
//
// The first record of a segment is always indexed, so a lookup always has a
// starting point better than the segment header.
func (s *segment) maybeIndex(rel uint32, position int64) error {
	if len(s.index.entries) > 0 && position-s.lastIndexed < indexIntervalBytes {
		return nil
	}
	if err := s.index.append(rel, position); err != nil {
		return err
	}
	s.lastIndexed = position
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

	rel := uint32(s.nextOffset - s.baseOffset)

	// Indexed after the record is written, never before. An index entry
	// pointing at bytes that were never written would survive a crash and send
	// a reader past the end of the file.
	if err := s.maybeIndex(rel, s.size); err != nil {
		return 0, err
	}

	offset := s.nextOffset
	s.size += int64(len(encoded))
	s.nextOffset++

	return offset, nil
}

// read returns the payload stored at offset.
//
// Seeks to the nearest indexed position at or before the target and scans
// forward. The scan is bounded by indexIntervalBytes of data, which is the
// cost traded for not holding a position per record in memory.
func (s *segment) read(offset uint64) ([]byte, error) {
	if offset < s.baseOffset || offset >= s.nextOffset {
		return nil, fmt.Errorf("%w: %d not in segment [%d,%d)", ErrOffsetOutOfRange, offset, s.baseOffset, s.nextOffset)
	}

	target := uint32(offset - s.baseOffset)

	position := int64(segmentHeaderSize)
	rel := uint32(0)
	if e, ok := s.index.lookup(target); ok {
		position = int64(e.position)
		rel = e.relOffset
	}

	// Walk forward record by record to the target.
	//
	// Buffered rather than a ReadAt per record. The first version did one
	// syscall per record header, which at ~57 records between index entries
	// meant ~57 syscalls per read and dominated the cost -- measured at
	// 86 microseconds per read before this change. A single buffered pass over
	// the interval reads it in one or two.
	if rel < target {
		scanner := bufio.NewReaderSize(
			io.NewSectionReader(s.file, position, s.size-position),
			indexIntervalBytes,
		)
		for rel < target {
			size, err := skipRecord(scanner)
			if err != nil {
				return nil, fmt.Errorf("scan toward offset %d: %w", offset, err)
			}
			position += size
			rel++
		}
	}

	// A SectionReader over the file rather than a Seek, because Seek mutates
	// shared file state and a concurrent reader would race with it. ReadAt,
	// which SectionReader uses, takes the position as an argument instead.
	//
	// The target record's checksum IS verified here. The headers skipped over
	// on the way are not, so a corrupt length in between surfaces either as a
	// read error or as a checksum failure on arrival rather than passing
	// silently.
	section := io.NewSectionReader(s.file, position, s.size-position)

	payload, _, err := readRecord(section)
	if err != nil {
		return nil, fmt.Errorf("read offset %d: %w", offset, err)
	}
	return payload, nil
}

// skipRecord advances r past one record and returns its total on-disk size.
//
// Only the header is parsed; the payload is discarded without being copied or
// checksummed. Skipping is not where correctness lives -- the target record's
// checksum is verified on arrival, so a corrupt length passed over here
// surfaces there as a read error or a checksum failure rather than silently.
func skipRecord(r *bufio.Reader) (int64, error) {
	header := make([]byte, recordHeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, err
	}

	length := binary.BigEndian.Uint32(header[0:lengthSize])
	if length > MaxRecordSize {
		return 0, fmt.Errorf("%w: length %d while scanning", ErrCorruptRecord, length)
	}
	if _, err := r.Discard(int(length)); err != nil {
		return 0, err
	}
	return int64(recordHeaderSize + int(length)), nil
}

// sync forces this segment's writes to durable storage.
//
// The segment is synced before the index: the segment is the authority, and an
// index describing records that did not survive is discarded on open, whereas
// records without index entries are simply rescanned.
func (s *segment) sync() error {
	if err := s.file.Sync(); err != nil {
		return err
	}
	return s.index.sync()
}

func (s *segment) close() error {
	err := s.file.Close()
	if indexErr := s.index.close(); err == nil {
		err = indexErr
	}
	return err
}
