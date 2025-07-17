//go:build linux

package v1

import (
	"context"
	"testing"

	"go.uber.org/zap/zaptest"
)

func TestIsSSLPacket(t *testing.T) {
	tests := []struct {
		name     string
		buffer   []byte
		expected bool
	}{
		{
			name:     "SSL handshake packet (TLS 1.2)",
			buffer:   []byte{0x16, 0x03, 0x03, 0x00, 0x40, 0x01, 0x00, 0x00},
			expected: true,
		},
		{
			name:     "SSL handshake packet (TLS 1.3)",
			buffer:   []byte{0x16, 0x03, 0x04, 0x00, 0x40, 0x01, 0x00, 0x00},
			expected: true,
		},
		{
			name:     "SSL request packet (should not be detected as SSL/TLS)",
			buffer:   []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xD2, 0x16, 0x2F},
			expected: false,
		},
		{
			name:     "Normal postgres packet",
			buffer:   []byte{0x00, 0x00, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00},
			expected: false,
		},
		{
			name:     "Too short buffer",
			buffer:   []byte{0x16, 0x03},
			expected: false,
		},
		{
			name:     "Random data",
			buffer:   []byte{0x12, 0x34, 0x56, 0x78, 0x9A, 0xBC, 0xDE, 0xF0},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSSLPacket(tt.buffer)
			if result != tt.expected {
				t.Errorf("isSSLPacket(%x) = %v, expected %v", tt.buffer, result, tt.expected)
			}
		})
	}
}

func TestIsValidPostgresPacket(t *testing.T) {
	tests := []struct {
		name      string
		buffer    []byte
		expected  bool
		errString string
	}{
		{
			name:     "Valid postgres protocol version 3.0",
			buffer:   []byte{0x00, 0x00, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00},
			expected: true,
		},
		{
			name:     "Valid SSL request",
			buffer:   []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xD2, 0x16, 0x2F},
			expected: true,
		},
		{
			name:      "SSL/TLS encrypted packet",
			buffer:    []byte{0x16, 0x03, 0x03, 0x00, 0x40, 0x01, 0x00, 0x00},
			expected:  false,
			errString: "detected SSL/TLS encrypted packet",
		},
		{
			name:      "Too short packet",
			buffer:    []byte{0x00, 0x00, 0x00},
			expected:  false,
			errString: "packet too short",
		},
		{
			name:     "Valid cancel request",
			buffer:   []byte{0x00, 0x00, 0x00, 0x10, 0x04, 0xD2, 0x16, 0x2E},
			expected: true,
		},
		{
			name:     "Valid GSS encryption request",
			buffer:   []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xD2, 0x16, 0x30},
			expected: true,
		},
		{
			name:      "Invalid message type",
			buffer:    []byte{0xFF, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00},
			expected:  false,
			errString: "invalid message type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := isValidPostgresPacket(tt.buffer)
			if result != tt.expected {
				t.Errorf("isValidPostgresPacket(%x) = %v, expected %v", tt.buffer, result, tt.expected)
			}
			if !tt.expected && tt.errString != "" {
				if err == nil {
					t.Errorf("Expected error containing '%s', got nil", tt.errString)
				} else if err.Error() == "" {
					t.Errorf("Expected error containing '%s', got empty error", tt.errString)
				}
			}
		})
	}
}

func TestMatchType(t *testing.T) {
	logger := zaptest.NewLogger(t)
	postgres := &PostgresV1{logger: logger}

	tests := []struct {
		name     string
		buffer   []byte
		expected bool
	}{
		{
			name:     "Valid postgres protocol",
			buffer:   []byte{0x00, 0x00, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00},
			expected: true,
		},
		{
			name:     "SSL request",
			buffer:   []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xD2, 0x16, 0x2F},
			expected: true,
		},
		{
			name:     "SSL/TLS encrypted packet",
			buffer:   []byte{0x16, 0x03, 0x03, 0x00, 0x40, 0x01, 0x00, 0x00},
			expected: false,
		},
		{
			name:     "Too short buffer",
			buffer:   []byte{0x00, 0x00},
			expected: false,
		},
		{
			name:     "Unknown version",
			buffer:   []byte{0x00, 0x00, 0x00, 0x08, 0xFF, 0xFF, 0xFF, 0xFF},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := postgres.MatchType(context.Background(), tt.buffer)
			if result != tt.expected {
				t.Errorf("MatchType(%x) = %v, expected %v", tt.buffer, result, tt.expected)
			}
		})
	}
}

func TestDecodePgRequestSafety(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tests := []struct {
		name       string
		buffer     []byte
		shouldPass bool // true if result should not be nil
	}{
		{
			name:       "Empty buffer",
			buffer:     []byte{},
			shouldPass: false,
		},
		{
			name:       "Too short buffer",
			buffer:     []byte{0x01, 0x02},
			shouldPass: false,
		},
		{
			name:       "Invalid message type",
			buffer:     []byte{0xFF, 0x00, 0x00, 0x00, 0x04, 0x00},
			shouldPass: false,
		},
		{
			name:       "Startup packet",
			buffer:     []byte{0x00, 0x00, 0x00, 0x08, 0x00, 0x03, 0x00, 0x00},
			shouldPass: false, // startup packets return nil in decodePgRequest
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := decodePgRequest(tt.buffer, logger)
			isNotNil := result != nil
			if isNotNil != tt.shouldPass {
				t.Errorf("decodePgRequest(%x) returned nil=%v, expected to pass=%v", tt.buffer, !isNotNil, tt.shouldPass)
			}
		})
	}
}
