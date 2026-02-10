package conn

import (
	"context"
	"encoding/binary"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func TestDecodeHandshakeResponse_BoundsChecking(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	tests := []struct {
		name          string
		data          []byte
		expectError   bool
		errorContains string
	}{
		{
			name:          "too short packet",
			data:          make([]byte, 10), // Less than minimum 32 bytes
			expectError:   true,
			errorContains: "handshake response packet too short",
		},
		{
			name: "minimum valid packet",
			data: func() []byte {
				// Create minimal valid handshake response
				data := make([]byte, 32)
				// Set CLIENT_PROTOCOL_41 capability
				binary.LittleEndian.PutUint32(data[:4], mysql.CLIENT_PROTOCOL_41)
				// Add null terminator for username at position 32
				data = append(data, 0x00)
				// Add auth response as null-terminated string (empty)
				data = append(data, 0x00)
				return data
			}(),
			expectError: false,
		},
		{
			name: "missing username null terminator",
			data: func() []byte {
				data := make([]byte, 32)
				binary.LittleEndian.PutUint32(data[:4], mysql.CLIENT_PROTOCOL_41)
				// No null terminator for username
				return append(data, []byte("username")...)
			}(),
			expectError:   true,
			errorContains: "missing null terminator for Username",
		},
		{
			name: "auth response - insufficient data for length (plugin auth)",
			data: func() []byte {
				data := make([]byte, 32)
				// CLIENT_PROTOCOL_41 + CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA
				binary.LittleEndian.PutUint32(data[:4],
					mysql.CLIENT_PROTOCOL_41|mysql.CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA,
				)
				// Add null terminator for username
				data = append(data, 0x00)
				// Don't add auth length byte
				return data
			}(),
			expectError:   true,
			errorContains: "handshake response packet too short for auth data length",
		},
		{
			name: "auth response - insufficient data for auth data (plugin auth)",
			data: func() []byte {
				data := make([]byte, 32)
				// CLIENT_PROTOCOL_41 + CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA
				binary.LittleEndian.PutUint32(data[:4],
					mysql.CLIENT_PROTOCOL_41|mysql.CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA,
				)
				// Add null terminator for username
				data = append(data, 0x00)
				// Add auth length = 5 but only provide 3 bytes
				data = append(data, 0x05)
				data = append(data, []byte{0x01, 0x02, 0x03}...)
				return data
			}(),
			expectError:   true,
			errorContains: "handshake response packet too short for auth data",
		},
		{
			name: "auth response - insufficient data for length (non-plugin auth)",
			data: func() []byte {
				data := make([]byte, 32)
				// CLIENT_PROTOCOL_41 + CLIENT_SECURE_CONNECTION
				binary.LittleEndian.PutUint32(data[:4],
					mysql.CLIENT_PROTOCOL_41|mysql.CLIENT_SECURE_CONNECTION,
				)
				// Add null terminator for username
				data = append(data, 0x00)
				// Don't add auth length byte
				return data
			}(),
			expectError:   true,
			errorContains: "handshake response packet too short for auth data length",
		},
		{
			name: "auth response - insufficient data for auth data (non-plugin auth)",
			data: func() []byte {
				data := make([]byte, 32)
				// CLIENT_PROTOCOL_41 + CLIENT_SECURE_CONNECTION
				binary.LittleEndian.PutUint32(data[:4],
					mysql.CLIENT_PROTOCOL_41|mysql.CLIENT_SECURE_CONNECTION,
				)
				// Add null terminator for username
				data = append(data, 0x00)
				// Add auth length = 5 but only provide 3 bytes
				data = append(data, 0x05)
				data = append(data, []byte{0x01, 0x02, 0x03}...) // Only 3 bytes (need 5)
				return data
			}(),
			expectError:   true,
			errorContains: "handshake response packet too short for auth data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("DecodeHandshakeResponse panicked: %v", r)
				}
			}()

			result, err := DecodeHandshakeResponse(ctx, logger, tt.data)

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if tt.errorContains != "" && !contains(err.Error(), tt.errorContains) {
					t.Errorf("Expected error to contain '%s', but got: %s", tt.errorContains, err.Error())
				}
				if result != nil {
					t.Errorf("Expected nil result on error, but got: %v", result)
				}
			} else {
				if err != nil {
					t.Errorf("Expected no error but got: %s", err.Error())
				}
				if result == nil {
					t.Errorf("Expected non-nil result but got nil")
				}
			}
		})
	}
}

func TestDecodeHandshakeResponse_ValidCases(t *testing.T) {
	logger := zap.NewNop()
	ctx := context.Background()

	tests := []struct {
		name     string
		data     []byte
		validate func(t *testing.T, result interface{})
	}{
		{
			name: "valid basic handshake",
			data: func() []byte {
				data := make([]byte, 32)
				// CLIENT_PROTOCOL_41
				binary.LittleEndian.PutUint32(data[:4], mysql.CLIENT_PROTOCOL_41)
				// Add username
				data = append(data, []byte("testuser")...)
				data = append(data, 0x00) // null terminator
				// Add auth response as null-terminated string (empty)
				data = append(data, 0x00)
				return data
			}(),
			validate: func(t *testing.T, result interface{}) {
				packet, ok := result.(*mysql.HandshakeResponse41Packet)
				if !ok {
					t.Errorf("Expected HandshakeResponse41Packet, got %T", result)
					return
				}
				if packet.Username != "testuser" {
					t.Errorf("Expected username 'testuser', got '%s'", packet.Username)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("DecodeHandshakeResponse panicked: %v", r)
				}
			}()

			result, err := DecodeHandshakeResponse(ctx, logger, tt.data)
			if err != nil {
				t.Errorf("Unexpected error: %s", err.Error())
				return
			}

			if tt.validate != nil {
				tt.validate(t, result)
			}
		})
	}
}

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
