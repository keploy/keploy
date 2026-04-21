package proxy

import "testing"

func TestIsPostgresSSLRequest(t *testing.T) {
	// The Postgres SSLRequest packet on the wire is exactly 8 bytes:
	//   int32 length = 8        → 00 00 00 08
	//   int32 code   = 80877103 → 04 d2 16 2f
	canonical := []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f}

	tests := []struct {
		name string
		buf  []byte
		want bool
	}{
		{"canonical", canonical, true},
		{"canonical with trailing bytes", append(canonical, 0xff, 0xff), true},
		{"too short — 7 bytes", canonical[:7], false},
		{"too short — empty", []byte{}, false},
		{"length wrong", []byte{0x00, 0x00, 0x00, 0x09, 0x04, 0xd2, 0x16, 0x2f}, false},
		{"code first byte wrong", []byte{0x00, 0x00, 0x00, 0x08, 0x05, 0xd2, 0x16, 0x2f}, false},
		{"code last byte wrong", []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x30}, false},
		{"all zero", make([]byte, 16), false},
		{"TLS ClientHello prefix", []byte{0x16, 0x03, 0x01, 0x00, 0xc8, 0x01, 0x00, 0x00}, false},
		{"HTTP GET prefix", []byte{'G', 'E', 'T', ' ', '/', ' ', 'H', 'T'}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isPostgresSSLRequest(tc.buf)
			if got != tc.want {
				t.Fatalf("isPostgresSSLRequest(%v) = %v, want %v", tc.buf, got, tc.want)
			}
		})
	}
}
