package mysql

import (
	"bytes"
	"context"
	"encoding/hex"
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

// validServerHandshakeV10 returns a minimal body that satisfies the greeting
// check: protocol version 0x0a, a NUL-terminated printable version string, and
// enough zero padding for the mandatory 31-byte tail. (It used to say the
// parser "doesn't care about the rest" — it does now.)
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

// realServerGreeting85 is a byte-accurate MySQL 8.0 HandshakeV10 body, laid out
// per wire/phase/conn/handshakeV10Packet.go's decoder: protocol version, a
// NUL-terminated server version, connection id, auth-plugin-data-part-1, the
// zero filler, capability flags, charset, status flags, auth-plugin-data-len,
// ten zero reserved bytes, then the rest of the salt and the plugin name.
func realServerGreeting85() []byte {
	body := []byte{0x0a}
	body = append(body, []byte("8.0.35")...)
	body = append(body, 0x00)                                   // version terminator
	body = append(body, 0x0b, 0x00, 0x00, 0x00)                 // connection id
	body = append(body, 'S', 'a', 'L', 't', '1', '2', '3', '4') // auth-plugin-data-part-1
	body = append(body, 0x00)                                   // filler — always zero
	body = append(body, 0xff, 0xf7)                             // capability flags (lower)
	body = append(body, 0xff)                                   // charset
	body = append(body, 0x02, 0x00)                             // status flags
	body = append(body, 0xff, 0xc0)                             // capability flags (upper)
	body = append(body, 0x15)                                   // auth-plugin-data-len
	body = append(body, make([]byte, 10)...)                    // reserved — all zero
	body = append(body, []byte("bcdefghijklm")...)              // auth-plugin-data-part-2
	body = append(body, 0x00)
	body = append(body, []byte("caching_sha2_password")...)
	body = append(body, 0x00)
	return body
}

// TestMySQL_MatchType_GreetingIsCorroborated covers the 0x0a branch, which used
// to return true on that single byte.
//
// That is not evidence of MySQL. A Postgres tagged frame is
// `tag | int32 BE length | payload`, so for any frame under 64 KiB the length's
// two high bytes are zero: the MySQL header read collapses pktLen to the ASCII
// tag byte (always inside the 5..512 gate) and seq to the length's high byte
// (<= 2 under 768 bytes). The whole header gate is defeated by construction and
// body[0] is all that is left — so every Postgres message whose total length
// ends in 0x0a was claimed as a MySQL greeting.
//
// This matcher is consulted precisely when the destination port is unknown, so
// content is the only signal there is, and a false claim routes the stream into
// the MySQL recorder.
func TestMySQL_MatchType_GreetingIsCorroborated(t *testing.T) {
	m := New(zap.NewNop()).(*MySQL)
	ctx := context.Background()

	t.Run("a real 8.0 greeting is still matched", func(t *testing.T) {
		pkt := buildMySQLPacket(0, realServerGreeting85())
		if !m.MatchType(ctx, pkt) {
			t.Fatal("rejected a genuine MySQL 8.0 HandshakeV10 — the corroboration is too strict, " +
				"and a rejected greeting means the connection falls to kind:Generic")
		}
	})

	t.Run("postgres frames ending in 0x0a are no longer claimed", func(t *testing.T) {
		// `tag | int32 BE length | payload`, total length chosen so the
		// low byte of the length is 0x0a — the shape that used to match.
		for _, total := range []int{267, 523} {
			for _, tag := range []byte{'Q', 'P', 'B', 'E', 'D'} {
				buf := make([]byte, total)
				buf[0] = tag
				l := uint32(total - 1)
				buf[1], buf[2], buf[3], buf[4] = byte(l>>24), byte(l>>16), byte(l>>8), byte(l)
				for i := 5; i < total; i++ {
					buf[i] = 'x'
				}
				if m.MatchType(ctx, buf) {
					t.Errorf("claimed a %d-byte Postgres %q frame as a MySQL greeting", total, tag)
				}
			}
		}
	})

	t.Run("arbitrary bytes behind a plausible header are not a greeting", func(t *testing.T) {
		for _, fill := range []byte{0xff, 0x00, 'A'} {
			body := make([]byte, 200)
			body[0] = 0x0a
			for i := 1; i < len(body); i++ {
				body[i] = fill
			}
			if m.MatchType(ctx, buildMySQLPacket(0, body)) {
				t.Errorf("claimed 200 bytes of 0x%02x behind a 0x0a as a MySQL greeting", fill)
			}
		}
	})

	t.Run("a body too short for the fixed tail is rejected", func(t *testing.T) {
		// NOT a model of wire truncation — the pre-existing `4+pktLen > len(buf)`
		// gate already rejects a real truncated capture, because the header still
		// declares the original length. This exercises the 31-byte tail check on
		// a body that is internally short, which is also what bounds the indexing.
		full := realServerGreeting85()
		for _, n := range []int{10, 16, 24, 30} {
			if n >= len(full) {
				continue
			}
			if m.MatchType(ctx, buildMySQLPacket(0, full[:n])) {
				t.Errorf("claimed a %d-byte body with no room for the greeting tail", n)
			}
		}
	})

	t.Run("a non-printable server version is rejected", func(t *testing.T) {
		body := realServerGreeting85()
		body[2] = 0x01 // inside the version string, before its NUL
		if m.MatchType(ctx, buildMySQLPacket(0, body)) {
			t.Error("claimed a greeting whose server version contains a control byte")
		}
	})

	t.Run("non-zero bytes in the checked reserved range are rejected", func(t *testing.T) {
		body := realServerGreeting85()
		end := bytes.IndexByte(body[1:], 0x00)
		rest := body[1+end+1:]
		for i := 21; i < 27; i++ {
			probe := append([]byte(nil), body...)
			pr := probe[1+end+1:]
			pr[i] = 0xAB
			if m.MatchType(ctx, buildMySQLPacket(0, probe)) {
				t.Errorf("claimed a greeting with reserved[%d] = 0xAB", i-21)
			}
		}
		_ = rest
	})
}

// realMariaDBGreeting1011 is a REAL greeting captured from mariadb:10.11
// (docker, MARIADB_ALLOW_EMPTY_ROOT_PASSWORD=1), not a reconstruction.
//
// It exists because MariaDB does NOT leave the ten "reserved" bytes zero: since
// 10.2 it writes its extended server capabilities into the last four. This
// capture's reserved block is 00 00 00 00 00 00 1d 00 00 00. A matcher that
// required all ten to be zero would reject every MariaDB server — and because
// mysql_detect.go caches a failed match for the session (LearnNotMysql), that
// rejection is permanent for the process and drops the connection back into the
// generic path this detection exists to avoid.
func realMariaDBGreeting1011() []byte {
	raw, err := hex.DecodeString("620000000a352e352e352d31302e31312e31392d4d6172696144422d756275323230340003000000314d6e63283a2d6d00fef72d0200ff81150000000000001d00000063283927555941754d60785c006d7973716c5f6e61746976655f70617373776f726400")
	if err != nil {
		panic(err)
	}
	return raw
}

// TestMySQL_MatchType_AcceptsRealServerGreetings guards the direction that
// matters most: a rejected genuine greeting is worse than the false positive
// being fixed, because mysql_detect.go remembers the rejection for the whole
// session.
func TestMySQL_MatchType_AcceptsRealServerGreetings(t *testing.T) {
	m := New(zap.NewNop()).(*MySQL)
	ctx := context.Background()

	t.Run("MariaDB 10.11 (non-zero extended caps in the reserved block)", func(t *testing.T) {
		if !m.MatchType(ctx, realMariaDBGreeting1011()) {
			t.Fatal("rejected a real MariaDB greeting — it uses the last four reserved bytes " +
				"for extended capabilities, and a rejection here is cached for the session")
		}
	})

	t.Run("MySQL 8.0", func(t *testing.T) {
		if !m.MatchType(ctx, buildMySQLPacket(0, realServerGreeting85())) {
			t.Fatal("rejected a MySQL 8.0 greeting")
		}
	})
}
