//go:build bigmem

package wire

// This row needs about 6 GiB of memory, so it is built only with the
// bigmem tag and run on a test box: go test -tags bigmem -run OverOneGiB.

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/rowscols"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A result set is many packets, each under MySQL's 16 MiB packet limit, so
// one answer may be more than 1 GiB. maxEncodedAnswer decides only whether
// EncodeToBinary sizes the framed copy up front: an answer above it is still
// served, and its bytes are the ones a smaller answer's framing gives. Here
// 72 rows of a 15 MiB value come to just over 1 GiB. The rows of the answer
// are checked against EncodeTextRow, and everything around them against the
// same result set encoded with no rows, so no second 1 GiB copy is held.
func TestEncodeToBinaryServesAnAnswerOverOneGiB(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	dc := &DecodeContext{LastOp: NewLastOpMap(), ServerGreetings: NewGreetings()}
	dc.ServerGreetings.Store(c1, &mysql.HandshakeV10Packet{CapabilityFlags: 0x81bff7ff})

	col := &mysql.ColumnDefinition41{Header: mysql.Header{PayloadLength: 30, SequenceID: 2}, Catalog: "def", Schema: "app", Table: "t", OrgTable: "t",
		Name: "c", OrgName: "c", FixedLength: 0x0c, CharacterSet: 33, ColumnLength: 255, Type: byte(mysql.FieldTypeVarString), Filler: []byte{0, 0}}
	const rows = 72
	const firstRowSeq = 4
	final := []byte{5, 0, 0, firstRowSeq + rows, 0xfe, 0, 0, 0x22, 0}
	resultSet := func(rowValue string, n int) *mysql.TextResultSet {
		rs := &mysql.TextResultSet{ColumnCount: 1, Columns: []*mysql.ColumnDefinition41{col}, EOFAfterColumns: []byte{5, 0, 0, 3, 0xfe, 0, 0, 2, 0},
			FinalResponse: &mysql.GenericResponse{Type: "EOF", Data: final}}
		for i := 0; i < n; i++ {
			rs.Rows = append(rs.Rows, &mysql.TextRow{Header: mysql.Header{SequenceID: byte(firstRowSeq + i)},
				Values: []mysql.ColumnEntry{{Type: mysql.FieldTypeVarString, Name: "c", Value: rowValue}}})
		}
		return rs
	}
	encode := func(rs *mysql.TextResultSet) []byte {
		t.Helper()
		pb := mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "TextResultSet", Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}}, Message: rs}
		out, err := EncodeToBinary(context.Background(), zap.NewNop(), &pb, c1, dc)
		if err != nil {
			t.Fatalf("an answer of %d rows was not encoded: %v", len(rs.Rows), err)
		}
		return out
	}

	value := strings.Repeat("x", 15<<20)
	row, err := rowscols.EncodeTextRow(context.Background(), zap.NewNop(), resultSet(value, 1).Rows[0], []*mysql.ColumnDefinition41{col})
	if err != nil {
		t.Fatal(err)
	}
	around := encode(resultSet(value, 0))
	head := around[:len(around)-len(final)]

	out := encode(resultSet(value, rows))
	if payload := len(out) - 4; payload <= maxEncodedAnswer {
		t.Fatalf("the answer's payload is %d bytes, not over maxEncodedAnswer (%d): the row no longer reaches the unsized copy", payload, maxEncodedAnswer)
	}
	if want := len(head) + rows*len(row) + len(final); len(out) != want {
		t.Fatalf("the answer is %d bytes, want %d", len(out), want)
	}
	if !bytes.Equal(out[:len(head)], head) {
		t.Fatalf("the answer's header and column packets are %x, want %x", out[:len(head)], head)
	}
	for i := 0; i < rows; i++ {
		got := out[len(head)+i*len(row):][:len(row)]
		if !bytes.Equal(got[:3], row[:3]) || got[3] != byte(firstRowSeq+i) || !bytes.Equal(got[4:], row[4:]) {
			t.Fatalf("row %d of the answer is not its encoded row: header %x, want %x with sequence %d", i, got[:4], row[:4], firstRowSeq+i)
		}
	}
	if tail := out[len(out)-len(final):]; !bytes.Equal(tail, final) {
		t.Fatalf("the answer ends %x, want %x", tail, final)
	}
}
