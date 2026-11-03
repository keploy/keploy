package wire

import (
	"bytes"
	"context"
	"net"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// The capabilities captured from MariaDB 10.6 and MariaDB Connector/C 3.4.9:
// server 0x81fff7fe with extended 0x1d, client 0x00bfa28c with extended 0x1d,
// CLIENT_MYSQL clear on both.
func TestMetadataFraming(t *testing.T) {
	both := mysql.FramingExtendedMetadata | mysql.FramingMetadataFollows
	for _, tc := range []struct {
		name                 string
		server, client       uint32
		serverExt, clientExt uint32
		recorded             uint32
		want                 mysql.MetadataFraming
	}{
		{"mariadb, connector/c", 0x81fff7fe, 0x00bfa28c, 0x1d, 0x1d, 0, both},
		{"mysql server", 0x81fff7ff, 0x00bfa28c, 0x1d, 0x1d, 0, 0},
		{"mysql client", 0x81fff7fe, 0x00bfa28d, 0x1d, 0x1d, 0, 0},
		{"server offers metadata only", 0x81fff7fe, 0x00bfa28c, 0x08, 0x1d, 0, mysql.FramingExtendedMetadata},
		{"client asks cache only", 0x81fff7fe, 0x00bfa28c, 0x1d, 0x10, 0, mysql.FramingMetadataFollows},
		{"no extended caps", 0x81fff7fe, 0x00bfa28c, 0, 0, 0, 0},
		// Replay frames by the recorded client's caps (PreferRecordedCaps).
		{"recorded client is mysql", 0x81fff7fe, 0x00bfa28c, 0x1d, 0x1d, 0x00bfa28d, 0},
	} {
		d := &DecodeContext{ServerCaps: tc.server, ClientCaps: tc.client, ServerMariaDBCaps: tc.serverExt,
			ClientMariaDBCaps: tc.clientExt, RecordedClientCaps: tc.recorded, PreferRecordedCaps: tc.recorded != 0}
		if got := d.MetadataFraming(); got != tc.want {
			t.Errorf("%s: MetadataFraming() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// A binary result set and a PREPARE_OK recorded on MariaDB are served framed
// by the connection they are sent on: on a MariaDB connection with Connector/C's
// extended capabilities, the column count packet carries its metadata-follows
// byte (and its header says 2 bytes) and each column definition its extended
// type information; on a MySQL connection, neither. One answer serves both
// (its cached bytes are its framing's).
func TestEncodeToBinaryFramesAResultSetByItsConnection(t *testing.T) {
	ctx, logger := context.Background(), zap.NewNop()
	conn := func(serverCaps, clientCaps, ext uint32) (*DecodeContext, net.Conn) {
		c1, c2 := net.Pipe()
		t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
		dc := &DecodeContext{LastOp: NewLastOpMap(), ServerGreetings: NewGreetings(), ServerCaps: serverCaps, ClientCaps: clientCaps,
			ServerMariaDBCaps: ext, ClientMariaDBCaps: ext}
		dc.ServerGreetings.Store(c1, &mysql.HandshakeV10Packet{CapabilityFlags: serverCaps, MariaDBCapabilities: ext})
		return dc, c1
	}
	mariaDB, mariaDBConn := conn(0x81fff7fe, 0x00bfa28c, 0x1d)
	mySQL, mySQLConn := conn(0x81fff7ff, 0x00bfa28d, 0)

	col := &mysql.ColumnDefinition41{Header: pktHeader(43, 2), Catalog: "def", Schema: "demo", Table: "t", OrgTable: "t", Name: "data", OrgName: "data",
		ExtendedMetadata: []mysql.ColumnExtendedMetadata{{Kind: 1, Value: "json"}},
		FixedLength:      0x0c, CharacterSet: 0x2d, ColumnLength: 0xffffffff, Type: byte(mysql.FieldTypeBLOB), Flags: 0x90, Filler: []byte{0, 0}}
	// The column definition's payload up to org_name, then after it, as
	// MariaDB 10.6 sent the JSON column.
	head := []byte("\x03def\x04demo\x01t\x01t\x04data\x04data")
	tail := []byte{0x0c, 0x2d, 0x00, 0xff, 0xff, 0xff, 0xff, 0xfc, 0x90, 0x00, 0x00, 0x00, 0x00}
	ext := []byte{0x06, 0x01, 0x04, 'j', 's', 'o', 'n'}
	packet := func(seq byte, payload ...[]byte) []byte {
		p := bytes.Join(payload, nil)
		return append([]byte{byte(len(p)), 0, 0, seq}, p...)
	}

	rs := &mysql.BinaryProtocolResultSet{ColumnCount: 1, Columns: []*mysql.ColumnDefinition41{col},
		EOFAfterColumns: []byte{5, 0, 0, 3, 0xfe, 0, 0, 0x22, 0},
		Rows: []*mysql.BinaryRow{{Header: pktHeader(0, 4), RowNullBuffer: []byte{0},
			Values: []mysql.ColumnEntry{{Type: mysql.FieldTypeBLOB, Name: "data", Value: `{"k":1}`}}}},
		FinalResponse: &mysql.GenericResponse{Type: "EOF", Data: []byte{5, 0, 0, 5, 0xfe, 0, 0, 0x22, 0}}}
	text := &mysql.TextResultSet{ColumnCount: 1, Columns: []*mysql.ColumnDefinition41{col},
		EOFAfterColumns: []byte{5, 0, 0, 3, 0xfe, 0, 0, 0x22, 0},
		Rows:            []*mysql.TextRow{{Header: pktHeader(0, 4), Values: []mysql.ColumnEntry{{Type: mysql.FieldTypeBLOB, Name: "data", Value: `{"k":1}`}}}},
		FinalResponse:   &mysql.GenericResponse{Type: "EOF", Data: []byte{5, 0, 0, 5, 0xfe, 0, 0, 0x22, 0}}}
	// Recorded on a MySQL connection: its header says 1 byte.
	header := func(typ string) *mysql.PacketInfo {
		return &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: typ}
	}
	for _, answer := range []*mysql.PacketBundle{
		{Header: header("BinaryProtocolResultSet"), Message: rs},
		{Header: header("TextResultSet"), Message: text},
	} {
		for _, tc := range []struct {
			name  string
			dc    *DecodeContext
			conn  net.Conn
			count []byte
			col   []byte
		}{
			{"mariadb", mariaDB, mariaDBConn, packet(1, []byte{1, 1}), packet(2, head, ext, tail)},
			{"mysql", mySQL, mySQLConn, packet(1, []byte{1}), packet(2, head, tail)},
			{"mariadb again", mariaDB, mariaDBConn, packet(1, []byte{1, 1}), packet(2, head, ext, tail)},
		} {
			got, err := EncodeToBinary(ctx, logger, answer, tc.conn, tc.dc)
			if err != nil {
				t.Fatalf("%s %s: %v", answer.Header.Type, tc.name, err)
			}
			want := append(append([]byte{}, tc.count...), tc.col...)
			if !bytes.HasPrefix(got, want) {
				t.Errorf("%s %s: served\n% x\nwant it to start\n% x", answer.Header.Type, tc.name, got, want)
			}
		}
	}

	prepare := &mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 1}, Type: mysql.COM_STMT_PREPARE_OK},
		Message: &mysql.StmtPrepareOkPacket{StatementID: 1, NumColumns: 1, NumParams: 1,
			ParamDefs: []*mysql.ColumnDefinition41{col}, EOFAfterParamDefs: []byte{5, 0, 0, 3, 0xfe, 0, 0, 2, 0},
			ColumnDefs: []*mysql.ColumnDefinition41{col}, EOFAfterColumnDefs: []byte{5, 0, 0, 5, 0xfe, 0, 0, 2, 0}}}
	for _, tc := range []struct {
		name string
		dc   *DecodeContext
		conn net.Conn
		col  []byte
	}{
		{"mariadb", mariaDB, mariaDBConn, bytes.Join([][]byte{head, ext, tail}, nil)},
		{"mysql", mySQL, mySQLConn, bytes.Join([][]byte{head, tail}, nil)},
	} {
		got, err := EncodeToBinary(ctx, logger, prepare, tc.conn, tc.dc)
		if err != nil {
			t.Fatalf("PREPARE_OK %s: %v", tc.name, err)
		}
		// The parameter's definition and the column's, each after its
		// header: its length, and a sequence id.
		if n := bytes.Count(got, tc.col); n != 2 || !bytes.Contains(got, []byte{byte(len(tc.col)), 0, 0}) {
			t.Errorf("PREPARE_OK %s: served\n% x\nwant both definitions as\n% x", tc.name, got, tc.col)
		}
	}
}
