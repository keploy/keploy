package utils

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestReadLengthEncodedInteger(t *testing.T) {
	tests := []struct {
		name         string
		input        []byte
		expectedNum  uint64
		expectedNull bool
		expectedN    int
		expectValid  bool
	}{
		// Test empty input
		{
			name:         "empty input",
			input:        []byte{},
			expectedNum:  0,
			expectedNull: true,
			expectedN:    0,
			expectValid:  true,
		},

		// Test single byte values (0-250)
		{
			name:         "single byte value 0",
			input:        []byte{0x00},
			expectedNum:  0,
			expectedNull: false,
			expectedN:    1,
			expectValid:  true,
		},
		{
			name:         "single byte value 250",
			input:        []byte{0xFA},
			expectedNum:  250,
			expectedNull: false,
			expectedN:    1,
			expectValid:  true,
		},

		// Test NULL value (251)
		{
			name:         "null value",
			input:        []byte{0xFB},
			expectedNum:  0,
			expectedNull: true,
			expectedN:    1,
			expectValid:  true,
		},

		// Test 2-byte values (252)
		{
			name:         "2-byte value with sufficient data",
			input:        []byte{0xFC, 0x01, 0x02},
			expectedNum:  0x0201, // 513 in little endian
			expectedNull: false,
			expectedN:    3,
			expectValid:  true,
		},
		{
			name:         "2-byte value with insufficient data (panic fix test)",
			input:        []byte{0xFC, 0x01}, // Missing one byte
			expectedNum:  0,
			expectedNull: true,
			expectedN:    0,
			expectValid:  true, // Should not panic, returns error state
		},
		{
			name:         "2-byte value with only marker (panic fix test)",
			input:        []byte{0xFC}, // Missing both bytes
			expectedNum:  0,
			expectedNull: true,
			expectedN:    0,
			expectValid:  true, // Should not panic, returns error state
		},

		// Test 3-byte values (253)
		{
			name:         "3-byte value with sufficient data",
			input:        []byte{0xFD, 0x01, 0x02, 0x03},
			expectedNum:  0x030201, // little endian
			expectedNull: false,
			expectedN:    4,
			expectValid:  true,
		},
		{
			name:         "3-byte value with insufficient data (panic fix test)",
			input:        []byte{0xFD, 0x01, 0x02}, // Missing one byte
			expectedNum:  0,
			expectedNull: true,
			expectedN:    0,
			expectValid:  true,
		},
		{
			name:         "3-byte value with only marker (panic fix test)",
			input:        []byte{0xFD}, // Missing all bytes
			expectedNum:  0,
			expectedNull: true,
			expectedN:    0,
			expectValid:  true,
		},

		// Test 8-byte values (254)
		{
			name:         "8-byte value with sufficient data",
			input:        []byte{0xFE, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08},
			expectedNum:  0x0807060504030201, // little endian
			expectedNull: false,
			expectedN:    9,
			expectValid:  true,
		},
		{
			name:         "8-byte value with insufficient data (panic fix test)",
			input:        []byte{0xFE, 0x01, 0x02, 0x03, 0x04}, // Missing 4 bytes
			expectedNum:  0,
			expectedNull: true,
			expectedN:    0,
			expectValid:  true,
		},
		{
			name:         "8-byte value with only marker (panic fix test)",
			input:        []byte{0xFE}, // Missing all bytes
			expectedNum:  0,
			expectedNull: true,
			expectedN:    0,
			expectValid:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test that it doesn't panic
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ReadLengthEncodedInteger panicked with input %v: %v", tt.input, r)
				}
			}()

			num, isNull, n := ReadLengthEncodedInteger(tt.input)

			if num != tt.expectedNum {
				t.Errorf("Expected num %d, got %d", tt.expectedNum, num)
			}
			if isNull != tt.expectedNull {
				t.Errorf("Expected isNull %t, got %t", tt.expectedNull, isNull)
			}
			if n != tt.expectedN {
				t.Errorf("Expected n %d, got %d", tt.expectedN, n)
			}
		})
	}
}

func TestReadLengthEncodedIntegerEdgeCases(t *testing.T) {
	// Test with malformed data that previously caused panics
	edgeCases := [][]byte{
		{0xFC},                   // 2-byte marker with no data
		{0xFC, 0x01},             // 2-byte marker with only 1 byte
		{0xFD},                   // 3-byte marker with no data
		{0xFD, 0x01},             // 3-byte marker with only 1 byte
		{0xFD, 0x01, 0x02},       // 3-byte marker with only 2 bytes
		{0xFE},                   // 8-byte marker with no data
		{0xFE, 0x01},             // 8-byte marker with only 1 byte
		{0xFE, 0x01, 0x02, 0x03}, // 8-byte marker with only 4 bytes
	}

	for i, data := range edgeCases {
		t.Run(fmt.Sprintf("edge_case_%d", i), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ReadLengthEncodedInteger panicked with edge case %d (data: %v): %v", i, data, r)
				}
			}()

			_, isNull, n := ReadLengthEncodedInteger(data)

			// Should return error state (isNull=true, n=0) for insufficient data
			if len(data) > 0 && data[0] >= 0xFC {
				if !isNull || n != 0 {
					t.Errorf("Expected error state (isNull=true, n=0) for insufficient data, got isNull=%t, n=%d", isNull, n)
				}
			}
		})
	}
}

// buildPacket constructs a MySQL wire-format packet: 3-byte payload length
// (little-endian) + 1-byte sequence ID + payload.
func buildPacket(seqID byte, payload []byte) []byte {
	pktLen := len(payload)
	return append([]byte{byte(pktLen), byte(pktLen >> 8), byte(pktLen >> 16), seqID}, payload...)
}

func TestIsEOFPacket(t *testing.T) {
	tests := []struct {
		name   string
		data   []byte
		expect bool
	}{
		{
			name:   "valid legacy EOF with CLIENT_PROTOCOL_41 (5-byte payload)",
			data:   buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00}),
			expect: true,
		},
		{
			name:   "valid minimal EOF (1-byte payload)",
			data:   buildPacket(1, []byte{0xFE}),
			expect: true,
		},
		{
			name:   "not EOF - wrong header byte (OK packet)",
			data:   buildPacket(1, []byte{0x00, 0x00, 0x00, 0x02, 0x00}),
			expect: false,
		},
		{
			name:   "not EOF - wrong header byte (ERR packet)",
			data:   buildPacket(1, []byte{0xFF, 0x48, 0x04, '#', '0'}),
			expect: false,
		},
		{
			name:   "not EOF - payload too large (7 bytes, OK-replacing-EOF)",
			data:   buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}),
			expect: false, // payload=7 is NOT a legacy EOF
		},
		{
			name:   "not EOF - payload too large (>9 bytes)",
			data:   buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03}),
			expect: false,
		},
		{
			name:   "not EOF - large packet with LSB=5 (false positive prevention)",
			data:   append([]byte{0x05, 0x01, 0x00, 0x01}, append([]byte{0xFE}, make([]byte, 260)...)...),
			expect: false, // payloadLen=0x0105=261, not 5
		},
		{
			name:   "too short - empty",
			data:   []byte{},
			expect: false,
		},
		{
			name:   "too short - 4 bytes",
			data:   []byte{0x01, 0x00, 0x00, 0x01},
			expect: false,
		},
		{
			name:   "truncated - header says 5 but only 2 payload bytes",
			data:   []byte{0x05, 0x00, 0x00, 0x01, 0xFE, 0x00},
			expect: false, // buffer too short for claimed payload
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsEOFPacket(tt.data)
			if got != tt.expect {
				t.Errorf("IsEOFPacket(%v) = %v, want %v", tt.data, got, tt.expect)
			}
		})
	}
}

func TestIsOKReplacingEOF(t *testing.T) {
	tests := []struct {
		name   string
		data   []byte
		expect bool
	}{
		{
			name:   "valid OK-replacing-EOF (minimal)",
			data:   buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}),
			expect: true,
		},
		{
			name:   "valid OK-replacing-EOF with session info",
			data:   buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x01, 0x02}),
			expect: true,
		},
		{
			name:   "legacy EOF (5-byte payload)",
			data:   buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00}),
			expect: false,
		},
		{
			name:   "wrong header byte",
			data:   buildPacket(1, []byte{0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}),
			expect: false,
		},
		{
			name:   "non-zero affected_rows",
			data:   buildPacket(1, []byte{0xFE, 0x01, 0x00, 0x02, 0x00, 0x00, 0x00}),
			expect: false,
		},
		{
			name:   "non-zero last_insert_id",
			data:   buildPacket(1, []byte{0xFE, 0x00, 0x01, 0x02, 0x00, 0x00, 0x00}),
			expect: false,
		},
		{
			name:   "too short packet",
			data:   []byte{0x03, 0x00, 0x00, 0x01, 0xFE, 0x00, 0x00},
			expect: false,
		},
		{
			name:   "inconsistent payload length",
			data:   []byte{0xFF, 0x00, 0x00, 0x01, 0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00},
			expect: false,
		},
		{
			name:   "nil data",
			data:   nil,
			expect: false,
		},
		{
			name:   "text row with 0xFE lenenc (8-byte integer, non-zero low bytes)",
			data:   buildPacket(1, []byte{0xFE, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}),
			expect: false, // affected_rows != 0
		},
		{
			name: "text row with 0xFE lenenc and zero low bytes (false-positive edge case)",
			data: func() []byte {
				// Simulate a text row where 0xFE lenenc has 0x00 0x00 as first
				// two bytes of the 8-byte integer, followed by large data.
				// The total payload exceeds maxOKReplacingEOFPayload.
				payload := make([]byte, 2000)
				payload[0] = 0xFE // lenenc marker
				payload[1] = 0x00 // low byte of integer
				payload[2] = 0x00 // second byte of integer
				// rest is data, all zeros
				return buildPacket(1, payload)
			}(),
			expect: false, // exceeds maxOKReplacingEOFPayload
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsOKReplacingEOF(tt.data)
			if got != tt.expect {
				t.Errorf("IsOKReplacingEOF(%v) = %v, want %v", tt.data, got, tt.expect)
			}
		})
	}
}

func TestIsResultSetTerminator(t *testing.T) {
	tests := []struct {
		name         string
		data         []byte
		deprecateEOF bool
		expect       bool
	}{
		{
			name:         "legacy EOF, deprecateEOF=false",
			data:         buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00}),
			deprecateEOF: false,
			expect:       true,
		},
		{
			name:         "legacy EOF, deprecateEOF=true",
			data:         buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00}),
			deprecateEOF: true,
			expect:       true,
		},
		{
			name:         "OK-replacing-EOF, deprecateEOF=true",
			data:         buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}),
			deprecateEOF: true,
			expect:       true,
		},
		{
			name:         "OK-replacing-EOF, deprecateEOF=false",
			data:         buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}),
			deprecateEOF: false,
			expect:       false, // OK-replacing-EOF should only terminate when deprecateEOF is negotiated
		},
		{
			name:         "OK-replacing-EOF with session info (>7 bytes), deprecateEOF=false",
			data:         buildPacket(1, []byte{0xFE, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03}),
			deprecateEOF: false,
			expect:       false, // Not legacy EOF (payload > 7), and deprecateEOF is false
		},
		{
			name:         "binary row (starts with 0x00), deprecateEOF=true",
			data:         buildPacket(1, []byte{0x00, 0x00, 0x01, 0x41}),
			deprecateEOF: true,
			expect:       false,
		},
		{
			name:         "text row, deprecateEOF=true",
			data:         buildPacket(1, []byte{0x05, 'h', 'e', 'l', 'l', 'o'}),
			deprecateEOF: true,
			expect:       false,
		},
		{
			name:         "ERR packet",
			data:         buildPacket(1, []byte{0xFF, 0x48, 0x04, '#', '2', '8', '0', '0', '0', 'e', 'r', 'r'}),
			deprecateEOF: false,
			expect:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsResultSetTerminator(tt.data, tt.deprecateEOF)
			if got != tt.expect {
				t.Errorf("IsResultSetTerminator(%v, %v) = %v, want %v", tt.data, tt.deprecateEOF, got, tt.expect)
			}
		})
	}
}

// TestParseBinaryDate_TruncatedBuffer guards the bounds-check fix for
// ParseBinaryDate. Before the fix, a non-zero length byte with a payload
// shorter than 4 bytes (e.g. a truncated DATE column in a malformed or
// fuzzed row packet) indexed past the end of b and panicked instead of
// returning a decode error.
func TestParseBinaryDate_TruncatedBuffer(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "length byte only", data: []byte{0x04}},
		{name: "missing day byte", data: []byte{0x04, 0x01, 0x02, 0x03}},
		// A garbage length byte (200) must not be trusted as the
		// bytes-consumed count: DecodeBinaryRow's row loop advances its
		// read offset by that count on the next column, so an
		// unvalidated value here becomes an out-of-range panic one
		// column later rather than in this function.
		{name: "non-canonical length byte", data: []byte{0xC8, 0x01, 0x02, 0x03, 0x04}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseBinaryDate panicked on %q: %v", tt.name, r)
				}
			}()
			_, _, err := ParseBinaryDate(tt.data)
			if err == nil {
				t.Fatalf("expected a decode error for %q, got nil", tt.name)
			}
		})
	}
}

// TestParseBinaryTime_TruncatedBuffer guards the bounds-check fix for
// ParseBinaryTime. Before the fix, a non-zero length byte with a payload
// shorter than 8 bytes (or shorter than 12 when microseconds are present)
// indexed past the end of b and panicked instead of returning a decode
// error.
func TestParseBinaryTime_TruncatedBuffer(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "length byte only", data: []byte{0x08}},
		{name: "missing seconds byte", data: []byte{0x08, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x02}},
		{name: "microseconds present but truncated", data: []byte{0x0C, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x04}},
		// A garbage length byte (240) must not be trusted as the
		// bytes-consumed count for the same reason as ParseBinaryDate
		// above - it would otherwise surface as a panic one column
		// later in DecodeBinaryRow's row loop. The buffer is padded to
		// 13 bytes (>= the microseconds floor) so this case is only
		// caught by the canonical-length check, not incidentally by
		// the floor check above.
		{name: "non-canonical length byte", data: []byte{0xF0, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x02, 0x03, 0x00, 0x00, 0x00, 0x00}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("ParseBinaryTime panicked on %q: %v", tt.name, r)
				}
			}()
			_, _, err := ParseBinaryTime(tt.data)
			if err == nil {
				t.Fatalf("expected a decode error for %q, got nil", tt.name)
			}
		})
	}
}

// TestReadLengthEncodedString_TruncatedExtendedPrefix guards the fix where
// ReadLengthEncodedString conflated a real NULL (the single byte 0xfb) with
// a truncated extended length-encoded-integer prefix (a lone 0xfc/0xfd/0xfe
// marker whose follow-up bytes are missing). Both used to report num < 1 and
// return a nil error; only n distinguishes them - a real NULL or a genuine
// empty string always consumes at least 1 byte (n >= 1), while a truncated
// prefix consumes none (n == 0).
func TestReadLengthEncodedString_TruncatedExtendedPrefix(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "0xfc with no follow-up bytes", data: []byte{0xfc}},
		{name: "0xfc with one of two follow-up bytes", data: []byte{0xfc, 0x01}},
		{name: "0xfd with follow-up bytes missing", data: []byte{0xfd, 0x01}},
		{name: "0xfe with follow-up bytes missing", data: []byte{0xfe, 0x01, 0x02}},
		{name: "empty buffer", data: []byte{}},
		// The other truncation path out of this function: the length prefix
		// reads fine, but the string body runs past the end of the packet.
		// Pre-existing on main and it returned the same bare io.EOF, so it is
		// covered here alongside the new branch.
		{name: "body declared longer than the buffer", data: []byte{0x05, 'a', 'b'}},
		{name: "0xfc body declared longer than the buffer", data: []byte{0xfc, 0x10, 0x00, 'a'}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, n, err := ReadLengthEncodedString(tt.data)
			if err == nil {
				t.Fatalf("expected an error for %q, got nil (n=%d)", tt.name, n)
			}
			// Which sentinel matters, not just that one was returned.
			// recorder/record_v2.go:110,142 treat io.EOF as a clean connection
			// close, so returning it here would make a malformed packet look
			// like the client hanging up: the recording would end quietly
			// instead of falling through to passthrough with a warning.
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("expected io.ErrUnexpectedEOF for %q, got %v", tt.name, err)
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("%q returned io.EOF, which upstream reads as a clean close", tt.name)
			}
		})
	}
}

// TestReadLengthEncodedString_RealNullAndEmptyString are the cases the fix
// above must not regress: an actual NULL marker and a genuine zero-length
// string both consume exactly 1 byte and must still succeed.
func TestReadLengthEncodedString_RealNullAndEmptyString(t *testing.T) {
	value, isNull, n, err := ReadLengthEncodedString([]byte{0xfb})
	if err != nil {
		t.Fatalf("NULL marker: unexpected error: %v", err)
	}
	if !isNull || n != 1 {
		t.Fatalf("NULL marker: got isNull=%v n=%d, want isNull=true n=1", isNull, n)
	}
	if len(value) != 0 {
		t.Fatalf("NULL marker: expected empty value, got %q", value)
	}

	value, isNull, n, err = ReadLengthEncodedString([]byte{0x00})
	if err != nil {
		t.Fatalf("empty string: unexpected error: %v", err)
	}
	if isNull || n != 1 {
		t.Fatalf("empty string: got isNull=%v n=%d, want isNull=false n=1", isNull, n)
	}
	if len(value) != 0 {
		t.Fatalf("empty string: expected empty value, got %q", value)
	}
}

func BenchmarkReadLengthEncodedInteger(b *testing.B) {
	testCases := []struct {
		name string
		data []byte
	}{
		{"single_byte", []byte{0x42}},
		{"two_bytes", []byte{0xFC, 0x01, 0x02}},
		{"three_bytes", []byte{0xFD, 0x01, 0x02, 0x03}},
		{"eight_bytes", []byte{0xFE, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				ReadLengthEncodedInteger(tc.data)
			}
		})
	}
}

// ReadNullTerminatedString sits one function below ReadLengthEncodedString and
// had the same bare-sentinel problem: its only caller is the handshake path
// (wire/phase/conn/authNextFactorPacket.go), whose error reaches the record
// loop's io.EOF check and is read there as the client closing cleanly.
func TestReadNullTerminatedString_NoTerminator(t *testing.T) {
	for _, tt := range []struct {
		name string
		data []byte
	}{
		{name: "no NUL byte", data: []byte{'a', 'b', 'c'}},
		{name: "empty buffer", data: []byte{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := ReadNullTerminatedString(tt.data)
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
			}
			if errors.Is(err, io.EOF) {
				t.Fatalf("returned io.EOF, which the record loop reads as a clean close")
			}
		})
	}
}
