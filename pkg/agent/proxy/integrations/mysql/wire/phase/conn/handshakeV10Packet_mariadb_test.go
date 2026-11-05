package conn

import (
	"context"
	"encoding/binary"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A MariaDB greeting's extended capabilities (captured from MariaDB 10.6:
// reserved 00000000000 01d000000) survive a decode and an encode, the
// greeting replay sends: without them, MariaDB Connector/C answers with none,
// and its HandshakeResponse41 no longer matches the one recorded.
func TestHandshakeV10_MariaDBCapabilitiesRoundTrip(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	in := &mysql.HandshakeV10Packet{
		ProtocolVersion:     10,
		ServerVersion:       "5.5.5-10.6.28-MariaDB",
		ConnectionID:        7,
		AuthPluginData:      []byte("abcdefghijklmnopqrst\x00"),
		CapabilityFlags:     0x81fff7fe, // CLIENT_MYSQL clear
		CharacterSet:        45,
		StatusFlags:         2,
		MariaDBCapabilities: 0x1d,
		AuthPluginName:      "mysql_native_password",
	}
	b, err := EncodeHandshakeV10(ctx, logger, in)
	if err != nil {
		t.Fatal(err)
	}
	// version, NUL, connection id, 8 auth bytes, filler, caps (2), charset,
	// status (2), caps (2), auth data length: then the ten reserved bytes.
	r := 1 + len(in.ServerVersion) + 1 + 4 + 8 + 1 + 2 + 1 + 2 + 2 + 1
	if got := binary.LittleEndian.Uint32(b[r+6 : r+10]); got != 0x1d {
		t.Fatalf("encoded reserved bytes %x, want MariaDB's capabilities 0x1d in the last four", b[r:r+10])
	}
	out, err := DecodeHandshakeV10(ctx, logger, b)
	if err != nil {
		t.Fatal(err)
	}
	if out.MariaDBCapabilities != 0x1d {
		t.Fatalf("decoded MariaDB capabilities 0x%x, want 0x1d", out.MariaDBCapabilities)
	}
}
