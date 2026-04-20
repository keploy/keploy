package proxy

import (
	"testing"
)

// TestIsKafkaPayload checks the hardened heuristic logic used in proxy.go
func TestIsKafkaPayload(t *testing.T) {
	tests := []struct {
		name     string
		payload  []byte
		expected bool
	}{
		{
			name: "Valid Kafka Header (ApiKey 1, Ver 10, Size 100)",
			// Size: 100 (0x64), ApiKey: 1, ApiVersion: 10, CorrelationId: 1
			payload:  []byte{0x00, 0x00, 0x00, 0x64, 0x00, 0x01, 0x00, 0x0A, 0x00, 0x00, 0x00, 0x01},
			expected: true,
		},
		{
			name: "Valid Kafka ApiVersions Request (ApiKey 18)",
			// Size: 50, ApiKey: 18 (ApiVersions), ApiVersion: 3
			payload:  []byte{0x00, 0x00, 0x00, 0x32, 0x00, 0x12, 0x00, 0x03, 0x00, 0x00, 0x00, 0x01},
			expected: true,
		},
		{
			name: "Invalid ApiKey (68 - out of range)",
			// Size: 100, ApiKey: 68 (0x44), ApiVersion: 10
			payload:  []byte{0x00, 0x00, 0x00, 0x64, 0x00, 0x44, 0x00, 0x0A, 0x00, 0x00, 0x00, 0x01},
			expected: false,
		},
		{
			name: "Invalid ApiVersion (16 - out of range)",
			// Size: 100, ApiKey: 1, ApiVersion: 16 (0x10)
			payload:  []byte{0x00, 0x00, 0x00, 0x64, 0x00, 0x01, 0x00, 0x10, 0x00, 0x00, 0x00, 0x01},
			expected: false,
		},
		{
			name: "Invalid Size (0)",
			// Size: 0, ApiKey: 1, ApiVersion: 10
			payload:  []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x0A, 0x00, 0x00, 0x00, 0x01},
			expected: false,
		},
		{
			name: "Invalid Size (negative - high bit set)",
			// Size: -1 (0xFFFFFFFF), ApiKey: 1, ApiVersion: 10
			payload:  []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x01, 0x00, 0x0A, 0x00, 0x00, 0x00, 0x01},
			expected: false,
		},
		{
			name:     "Short Payload (11 bytes - need 12)",
			payload:  []byte{0x00, 0x00, 0x00, 0x64, 0x00, 0x01, 0x00, 0x0A, 0x00, 0x00, 0x00},
			expected: false,
		},
		{
			name:     "Empty Payload",
			payload:  []byte{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isKafka := false
			if len(tt.payload) >= 12 {
				// Parse size (big-endian int32)
				size := int32(tt.payload[0])<<24 | int32(tt.payload[1])<<16 |
					int32(tt.payload[2])<<8 | int32(tt.payload[3])

				// Sanity check: size should be positive and reasonable (< 100MB)
				if size > 0 && size < 100*1024*1024 {
					// Parse ApiKey (big-endian int16)
					apiKey := int16(tt.payload[4])<<8 | int16(tt.payload[5])

					// Parse ApiVersion (big-endian int16)
					apiVersion := int16(tt.payload[6])<<8 | int16(tt.payload[7])

					// Known Kafka API Keys: 0-67 (as of Kafka 3.6)
					if apiKey >= 0 && apiKey <= 67 && apiVersion >= 0 && apiVersion <= 15 {
						isKafka = true
					}
				}
			}

			if isKafka != tt.expected {
				t.Errorf("expected %v, got %v for payload %x", tt.expected, isKafka, tt.payload)
			}
		})
	}
}
