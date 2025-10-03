package utils

import (
	"fmt"
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
