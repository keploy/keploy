package mysql

import (
	"context"
	"fmt"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.uber.org/zap"
)

// TestPanicFixes demonstrates that the previous panic-causing conditions now return errors instead
func TestPanicFixes(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	// Test cases that would have caused panics before the fix
	panicCausingInputs := []struct {
		name string
		test func(t *testing.T)
	}{
		{
			name: "ReadLengthEncodedInteger with insufficient bytes for 2-byte value",
			test: func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("ReadLengthEncodedInteger panicked: %v", r)
					}
				}()

				// This would have caused a panic before the fix
				_, isNull, n := utils.ReadLengthEncodedInteger([]byte{0xFC, 0x01}) // Missing one byte

				// Should return error state (isNull=true, n=0)
				if !isNull || n != 0 {
					t.Errorf("Expected error state for insufficient bytes, got isNull=%t, n=%d", isNull, n)
				}
			},
		},
		{
			name: "ReadLengthEncodedInteger with insufficient bytes for 3-byte value",
			test: func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("ReadLengthEncodedInteger panicked: %v", r)
					}
				}()

				// This would have caused a panic before the fix
				_, isNull, n := utils.ReadLengthEncodedInteger([]byte{0xFD, 0x01, 0x02}) // Missing one byte

				// Should return error state
				if !isNull || n != 0 {
					t.Errorf("Expected error state for insufficient bytes, got isNull=%t, n=%d", isNull, n)
				}
			},
		},
		{
			name: "ReadLengthEncodedInteger with insufficient bytes for 8-byte value",
			test: func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("ReadLengthEncodedInteger panicked: %v", r)
					}
				}()

				// This would have caused a panic before the fix
				_, isNull, n := utils.ReadLengthEncodedInteger([]byte{0xFE, 0x01, 0x02, 0x03, 0x04}) // Missing 4 bytes

				// Should return error state
				if !isNull || n != 0 {
					t.Errorf("Expected error state for insufficient bytes, got isNull=%t, n=%d", isNull, n)
				}
			},
		},
		{
			name: "DecodeHandshakeResponse with malformed connection attributes",
			test: func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("DecodeHandshakeResponse panicked: %v", r)
					}
				}()

				// Create a packet that would have caused a panic in connection attributes parsing
				data := make([]byte, 32)
				// Set CLIENT_PROTOCOL_41 (0x200) + CLIENT_CONNECT_ATTRS (0x80000)
				data[0] = 0x00
				data[1] = 0x02
				data[2] = 0x08 // CLIENT_CONNECT_ATTRS
				data[3] = 0x00

				// Add null terminator for username
				data = append(data, 0x00)
				// Add auth response
				data = append(data, 0x00, 0x00) // length + filler
				// Add connection attributes with insufficient data - this would have caused panic
				data = append(data, 0x0A)                  // total length = 10
				data = append(data, 0x05)                  // key length = 5
				data = append(data, []byte{0x01, 0x02}...) // Only 2 bytes for key (need 5)

				// This should return an error, not panic
				result, err := conn.DecodeHandshakeResponse(ctx, logger, data)

				if err == nil {
					t.Errorf("Expected error but got none")
				}
				if result != nil {
					t.Errorf("Expected nil result on error")
				}
			},
		},
		{
			name: "DecodeHandshakeResponse with auth response overflow",
			test: func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("DecodeHandshakeResponse panicked: %v", r)
					}
				}()

				// Create a packet that would have caused panic in auth response parsing
				data := make([]byte, 32)
				// CLIENT_PROTOCOL_41 only (non-plugin auth)
				data[0] = 0x00
				data[1] = 0x02
				data[2] = 0x00
				data[3] = 0x00

				// Add username
				data = append(data, []byte("user")...)
				data = append(data, 0x00) // null terminator
				// Add auth response with overflow - this would have caused panic
				data = append(data, 0x10, 0x00)                  // auth length = 16 + filler
				data = append(data, []byte{0x01, 0x02, 0x03}...) // Only 3 bytes (need 16)

				// This should return an error, not panic
				result, err := conn.DecodeHandshakeResponse(ctx, logger, data)

				if err == nil {
					t.Errorf("Expected error but got none")
				}
				if result != nil {
					t.Errorf("Expected nil result on error")
				}
			},
		},
	}

	for _, tc := range panicCausingInputs {
		t.Run(tc.name, tc.test)
	}
}

// TestFuzzingResilience tests that random malformed data doesn't cause panics
func TestFuzzingResilience(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	// Test with various sizes of random/malformed data
	testCases := [][]byte{
		{},                             // Empty
		{0xFF},                         // Single byte
		{0xFC},                         // 2-byte marker only
		{0xFD},                         // 3-byte marker only
		{0xFE},                         // 8-byte marker only
		{0xFC, 0xFF},                   // 2-byte marker with 1 byte
		{0xFD, 0xFF, 0xFF},             // 3-byte marker with 2 bytes
		{0xFE, 0xFF, 0xFF, 0xFF, 0xFF}, // 8-byte marker with 4 bytes
		make([]byte, 31),               // Just under minimum handshake size
		make([]byte, 100),              // Large buffer with zeros
	}

	for i, data := range testCases {
		t.Run(fmt.Sprintf("fuzz_case_%d", i), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("Unexpected panic with fuzz data %d: %v", i, r)
				}
			}()

			// Test ReadLengthEncodedInteger
			utils.ReadLengthEncodedInteger(data)

			// Test DecodeHandshakeResponse
			conn.DecodeHandshakeResponse(ctx, logger, data)
		})
	}
}
