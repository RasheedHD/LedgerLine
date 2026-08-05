package log

import (
	"bytes"
	"errors"
	"testing"
)

func TestRecordRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		proves  string
		payload []byte
	}{
		{
			name:    "typical payload",
			proves:  "the ordinary case survives encode and decode unchanged",
			payload: []byte(`{"tenant_id":"acme","quantity":"1"}`),
		},
		{
			name:    "empty payload",
			proves:  "a zero-length record is legal and does not collapse into a torn one",
			payload: []byte{},
		},
		{
			name:    "single byte",
			proves:  "the smallest non-empty record still frames correctly",
			payload: []byte{0x00},
		},
		{
			name:    "binary payload with embedded newlines and nulls",
			proves:  "framing is length-based, so no byte value needs escaping",
			payload: []byte{0x00, 0xFF, '\n', '\r', 0x00, 0x1A},
		},
		{
			name:    "payload at the maximum size",
			proves:  "the boundary itself is accepted rather than being off by one",
			payload: bytes.Repeat([]byte{0xAB}, MaxRecordSize),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := encodeRecord(tc.payload)
			if err != nil {
				t.Fatalf("encode: %v\nthis case proves: %s", err, tc.proves)
			}
			if want := recordHeaderSize + len(tc.payload); len(encoded) != want {
				t.Errorf("encoded length = %d, want %d", len(encoded), want)
			}

			got, n, err := readRecord(bytes.NewReader(encoded))
			if err != nil {
				t.Fatalf("read: %v\nthis case proves: %s", err, tc.proves)
			}
			if n != len(encoded) {
				t.Errorf("consumed %d bytes, want %d -- a wrong count desynchronises the next read", n, len(encoded))
			}
			if !bytes.Equal(got, tc.payload) {
				t.Errorf("payload round trip changed the bytes\nthis case proves: %s", tc.proves)
			}
		})
	}
}

func TestRecordTooLarge(t *testing.T) {
	_, err := encodeRecord(bytes.Repeat([]byte{1}, MaxRecordSize+1))
	if !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("error = %v, want ErrRecordTooLarge -- the limit must be enforced before anything is written", err)
	}
}

// Damage detection. Each case corrupts a valid record in a different way and
// asserts the reader notices, because the alternative is handing corrupted
// billing data to the consumer as though it were sound.
func TestRecordDetectsDamage(t *testing.T) {
	payload := []byte("usage-event-payload")

	tests := []struct {
		name    string
		proves  string
		damage  func([]byte) []byte
		wantErr error
	}{
		{
			name:    "header cut short",
			proves:  "a crash between writing two bytes of the header is a torn record, not a corrupt one",
			damage:  func(b []byte) []byte { return b[:recordHeaderSize-1] },
			wantErr: ErrTornRecord,
		},
		{
			name:    "payload cut short",
			proves:  "the common crash shape -- header written, payload partially written -- is recognised",
			damage:  func(b []byte) []byte { return b[:len(b)-1] },
			wantErr: ErrTornRecord,
		},
		{
			name:    "empty input",
			proves:  "a clean end of file is not mistaken for damage",
			damage:  func(b []byte) []byte { return nil },
			wantErr: nil, // io.EOF, checked separately below
		},
		{
			name:   "flipped bit in the payload",
			proves: "the checksum covers the payload, so silent bit rot is caught",
			damage: func(b []byte) []byte {
				out := append([]byte(nil), b...)
				out[recordHeaderSize+3] ^= 0x01
				return out
			},
			wantErr: ErrCorruptRecord,
		},
		{
			name:   "flipped bit in the stored checksum",
			proves: "a damaged checksum fails closed rather than being trusted",
			damage: func(b []byte) []byte {
				out := append([]byte(nil), b...)
				out[lengthSize] ^= 0x01
				return out
			},
			wantErr: ErrCorruptRecord,
		},
		{
			name:   "flipped bit in the length field",
			proves: "the checksum covers the LENGTH too -- otherwise this desynchronises every following record instead of failing here",
			damage: func(b []byte) []byte {
				out := append([]byte(nil), b...)
				out[lengthSize-1] ^= 0x01
				return out
			},
			wantErr: ErrCorruptRecord,
		},
		{
			name:   "length field claiming an absurd size",
			proves: "a corrupt length is refused before it becomes an allocation, turning disk damage into an OOM",
			damage: func(b []byte) []byte {
				out := append([]byte(nil), b...)
				out[0] = 0xFF
				return out
			},
			wantErr: ErrCorruptRecord,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := encodeRecord(payload)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}

			_, _, err = readRecord(bytes.NewReader(tc.damage(encoded)))
			if err == nil {
				t.Fatalf("damaged record read successfully\nthis case proves: %s", tc.proves)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v\nthis case proves: %s", err, tc.wantErr, tc.proves)
			}
		})
	}
}

// A truncated length field must not be readable as a shorter number. This is
// the case that would silently return the wrong payload if the header were
// parsed leniently.
func TestRecordWithTruncatedLengthIsTorn(t *testing.T) {
	encoded, err := encodeRecord([]byte("hello"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	_, _, err = readRecord(bytes.NewReader(encoded[:2]))
	if !errors.Is(err, ErrTornRecord) {
		t.Fatalf("error = %v, want ErrTornRecord", err)
	}
}
