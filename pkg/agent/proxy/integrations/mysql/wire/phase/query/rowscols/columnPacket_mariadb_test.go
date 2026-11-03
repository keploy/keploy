package rowscols

import (
	"bytes"
	"context"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// Column definitions captured from MariaDB 10.6.28 answering mysqlclient on
// MariaDB Connector/C 3.4.9, a connection that negotiated
// MARIADB_CLIENT_EXTENDED_METADATA: an INT column (empty extended type
// information, one 0x00 byte) and a JSON column (format "json").
const (
	mariaDBIntColumn  = "036465660464656d6f01740174026964026964000c3f000b000000030350000000"
	mariaDBJSONColumn = "036465660464656d6f01740174046461746104646174610601046a736f6e0c2d00fffffffffc9000000000"
)

func packet(t *testing.T, payloadHex string, seq byte) []byte {
	t.Helper()
	p, err := hex.DecodeString(payloadHex)
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), seq}, p...)
}

func TestDecodeColumn_MariaDBExtendedMetadata(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	for _, tc := range []struct {
		name    string
		column  string
		payload string
		ext     []mysql.ColumnExtendedMetadata
		typ     byte
		charset uint16
		length  uint32
		flags   uint16
	}{
		{"int", "id", mariaDBIntColumn, nil, 0x03, 63, 11, 0x5003},
		{"json", "data", mariaDBJSONColumn, []mysql.ColumnExtendedMetadata{{Kind: 1, Value: "json"}}, 0xfc, 45, 0xffffffff, 0x0090},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := packet(t, tc.payload, 2)
			col, pos, err := DecodeColumn(ctx, logger, b, mysql.FramingExtendedMetadata)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if pos != len(b) {
				t.Errorf("decode read %d of the packet's %d bytes", pos, len(b))
			}
			if !reflect.DeepEqual(col.ExtendedMetadata, tc.ext) || col.Type != tc.typ || col.CharacterSet != tc.charset ||
				col.ColumnLength != tc.length || col.Flags != tc.flags || col.DefaultValue != "" || col.Name != tc.column {
				t.Errorf("decoded %+v", col)
			}
			// Replay on a connection of the same framing sends the bytes the server sent.
			enc, err := EncodeColumn(ctx, logger, col, mysql.FramingExtendedMetadata)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			if !bytes.Equal(enc, b) {
				t.Errorf("encoded\n %x\nwant\n %x", enc, b)
			}
		})
	}
}

// Read without its extended type information, a MariaDB column definition's
// fields shift by the bytes of that information: the decoder refuses it
// instead of recording the shifted fields (an INT read so had character set
// 0x3f0c and type 0x00) or a 255-byte default value.
func TestDecodeColumn_MariaDBLayoutReadAsMySQLFailsClosed(t *testing.T) {
	for _, p := range []string{mariaDBIntColumn, mariaDBJSONColumn} {
		if col, _, err := DecodeColumn(context.Background(), zap.NewNop(), packet(t, p, 2), 0); err == nil {
			t.Errorf("decoded %x as a MySQL column definition: %+v", p, col)
		}
	}
}

// Sent on a connection that did not negotiate extended metadata, a column
// recorded with it is a MySQL column definition, which decodes back.
func TestEncodeColumn_WithoutExtendedMetadata(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	col, _, err := DecodeColumn(ctx, logger, packet(t, mariaDBJSONColumn, 4), mysql.FramingExtendedMetadata)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := EncodeColumn(ctx, logger, col, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := packet(t, "036465660464656d6f0174017404646174610464617461"+"0c2d00fffffffffc9000000000", 4)
	if !bytes.Equal(enc, want) {
		t.Fatalf("encoded\n %x\nwant\n %x", enc, want)
	}
	back, _, err := DecodeColumn(ctx, logger, enc, 0)
	if err != nil || back.Type != 0xfc || back.ExtendedMetadata != nil {
		t.Fatalf("decoded back %+v, %v", back, err)
	}
}

// Extended type information that does not fit is refused, and the reason
// says which part did not: the recorder leaves the exchange out with it.
func TestDecodeColumn_ExtendedMetadataOverrunsPacket(t *testing.T) {
	for _, tc := range []struct{ name, payload, want string }{
		// The JSON column with its extended type information's length
		// raised from 6 to 0x40: past the end of the packet.
		{"length past the packet", "036465660464656d6f01740174046461746104646174614001046a736f6e0c2d00fffffffffc9000000000", "(64 bytes) overruns the packet"},
		// An entry whose value's length runs past the information's own.
		{"entry past its information", "036465660464656d6f0174017404646174610464617461030105" + "0c2d00fffffffffc9000000000", "extended type information does not decode"},
		// The packet ends after org_name, where the information belongs.
		{"packet ends before it", "036465660464656d6f0174017404646174610464617461", "truncated before its extended type information"},
	} {
		_, _, err := DecodeColumn(context.Background(), zap.NewNop(), packet(t, tc.payload, 2), mysql.FramingExtendedMetadata)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: DecodeColumn = %v, want an error saying %q", tc.name, err, tc.want)
		}
	}
}

// Under MariaDB's metadata cache a column count is followed by its
// metadata-follows byte (captured: 04 01 for four columns).
func TestDecodeColumnCount_MetadataFollows(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	for _, tc := range []struct {
		data    []byte
		framing mysql.MetadataFraming
		want    uint64
		ok      bool
	}{
		{[]byte{4, 1}, mysql.FramingMetadataFollows, 4, true},
		{[]byte{4, 0}, mysql.FramingMetadataFollows, 0, false}, // definitions left out
		{[]byte{4}, mysql.FramingMetadataFollows, 0, false},    // byte missing
		{[]byte{4}, 0, 4, true},
		{[]byte{0xfc, 0x2c, 0x01, 1}, mysql.FramingMetadataFollows | mysql.FramingExtendedMetadata, 300, true},
	} {
		got, err := DecodeColumnCount(ctx, logger, tc.data, tc.framing)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("DecodeColumnCount(%x, %d) = %d, %v; want %d, ok %v", tc.data, tc.framing, got, err, tc.want, tc.ok)
		}
	}
}
