package mysql

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

// buildMySQLPacket assembles a single MySQL framed packet:
//
//	3-byte little-endian length | 1-byte sequence | body
func buildMySQLPacket(seq uint8, body []byte) []byte {
	pkt := make([]byte, 4+len(body))
	pkt[0] = byte(len(body))
	pkt[1] = byte(len(body) >> 8)
	pkt[2] = byte(len(body) >> 16)
	pkt[3] = seq
	copy(pkt[4:], body)
	return pkt
}

// validClientHandshakeResponse41 returns a body that satisfies the
// content-detect heuristic: capability flags with CLIENT_PROTOCOL_41 |
// CLIENT_PLUGIN_AUTH set, max_packet_size + charset, and a 23-byte
// zero-filled reserved field (offset 9..32).
func validClientHandshakeResponse41() []byte {
	body := make([]byte, 32)
	// caps: CLIENT_PROTOCOL_41 (0x00000200) | CLIENT_PLUGIN_AUTH (0x00080000)
	body[0] = 0x00
	body[1] = 0x02
	body[2] = 0x08
	body[3] = 0x00
	// max_packet_size = 16M
	body[4] = 0x00
	body[5] = 0x00
	body[6] = 0x00
	body[7] = 0x01
	// charset
	body[8] = 0x21
	// body[9..32] are the 23 reserved zero bytes — already zero.
	return body
}

// validServerHandshakeV10 returns a body whose first byte is the
// protocol version 10 (0x0a). The MySQL parser doesn't care about
// the rest, so we pad with arbitrary printable bytes.
func validServerHandshakeV10() []byte {
	body := []byte{0x0a, '8', '.', '0', '.', '3', '5', 0x00, 0x01, 0x00, 0x00, 0x00, 'A', 'B', 'C', 'D'}
	for len(body) < 50 {
		body = append(body, 0x00)
	}
	return body
}

func TestMySQL_MatchType(t *testing.T) {
	m := New(zap.NewNop()).(*MySQL)

	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		{
			name:  "buffer too short",
			input: []byte{0x00, 0x00, 0x00, 0x08},
			want:  false,
		},
		{
			name:  "valid server HandshakeV10 at seq 0",
			input: buildMySQLPacket(0, validServerHandshakeV10()),
			want:  true,
		},
		{
			name:  "valid client HandshakeResponse41 at seq 1",
			input: buildMySQLPacket(1, validClientHandshakeResponse41()),
			want:  true,
		},
		{
			name:  "valid client HandshakeResponse41 at seq 2",
			input: buildMySQLPacket(2, validClientHandshakeResponse41()),
			want:  true,
		},
		{
			name:  "client handshake but seq 3 — rejected",
			input: buildMySQLPacket(3, validClientHandshakeResponse41()),
			want:  false,
		},
		{
			name:  "client handshake but reserved bytes non-zero",
			input: buildMySQLPacket(1, func() []byte { b := validClientHandshakeResponse41(); b[15] = 0xff; return b }()),
			want:  false,
		},
		{
			name:  "client handshake missing CLIENT_PROTOCOL_41 bit",
			input: buildMySQLPacket(1, func() []byte { b := validClientHandshakeResponse41(); b[1] = 0x00; return b }()),
			want:  false,
		},
		{
			name:  "client handshake missing CLIENT_PLUGIN_AUTH bit",
			input: buildMySQLPacket(1, func() []byte { b := validClientHandshakeResponse41(); b[2] = 0x00; return b }()),
			want:  false,
		},
		{
			name:  "TLS ClientHello first 16 bytes",
			input: []byte{0x16, 0x03, 0x01, 0x00, 0xc8, 0x01, 0x00, 0x00, 0xc4, 0x03, 0x03, 0xa1, 0xb2, 0xc3, 0xd4, 0xe5},
			want:  false,
		},
		{
			name:  "HTTP GET request first 16 bytes",
			input: []byte("GET / HTTP/1.1\r\n"),
			want:  false,
		},
		{
			name:  "HTTP POST with small body",
			input: []byte("POST /api HTTP/1.1\r\nContent-Length: 0\r\n\r\n"),
			want:  false,
		},
		{
			name:  "MongoDB OP_MSG header 16 bytes (length=78)",
			input: []byte{0x4e, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xdd, 0x07, 0x00, 0x00},
			want:  false,
		},
		{
			name:  "Postgres SSLRequest 8 bytes padded",
			input: []byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x2f, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
			want:  false,
		},
		{
			name:  "all zero bytes",
			input: make([]byte, 64),
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := m.MatchType(context.Background(), tc.input)
			if got != tc.want {
				t.Fatalf("MatchType = %v, want %v (input len=%d)", got, tc.want, len(tc.input))
			}
		})
	}
}
