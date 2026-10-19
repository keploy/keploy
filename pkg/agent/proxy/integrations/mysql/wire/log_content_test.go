package wire

import (
	"context"
	"encoding/hex"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

const canary = "kp-canary-mysql-wire-5e2a"

func pipeCtx(t *testing.T, mode models.Mode) (net.Conn, *DecodeContext) {
	t.Helper()
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	decodeCtx := &DecodeContext{Mode: mode, LastOp: NewLastOpMap(), ServerGreetings: NewGreetings()}
	decodeCtx.ServerGreetings.Store(client, &mysql.HandshakeV10Packet{CapabilityFlags: mysql.CLIENT_PROTOCOL_41})
	return client, decodeCtx
}

// A replayed result set is logged as its bytes' length and fingerprint, not
// its rows, unless KEPLOY_LOG_CONTENT is set.
func TestEncodeToBinary_TheEncodedPacketLineCarriesNoRow(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		client, decodeCtx := pipeCtx(t, models.MODE_TEST)
		col := &mysql.ColumnDefinition41{Header: mysql.Header{SequenceID: 2}, Catalog: "def", Name: "note", OrgName: "note",
			FixedLength: 0x0c, CharacterSet: 0x21, ColumnLength: 256, Type: 0xfd, Filler: []byte{0, 0}}
		rs := &mysql.TextResultSet{
			ColumnCount: 1,
			Columns:     []*mysql.ColumnDefinition41{col},
			Rows: []*mysql.TextRow{{Header: mysql.Header{SequenceID: 3},
				Values: []mysql.ColumnEntry{{Type: mysql.FieldTypeVarString, Value: canary}}}},
			FinalResponse: &mysql.GenericResponse{Type: "OK", Data: []byte{0x07, 0x00, 0x00, 0x04, 0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}},
		}
		logger, logs := logtest.Observe()
		bundle := &mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: "TextResultSet"}, Message: rs}
		if _, err := EncodeToBinary(context.Background(), logger, bundle, client, decodeCtx); err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on, "Encoded Packet")
	})
}

// A binary result set's final packet, an OK in place of the EOF, can carry
// the server's info text: it is logged as its length and fingerprint, unless
// KEPLOY_LOG_CONTENT is set.
func TestEncodeToBinary_TheFinalResponseLineCarriesNoInfo(t *testing.T) {
	info := []byte(canary)
	final := append([]byte{byte(7 + len(info)), 0x00, 0x00, 0x05, 0xfe, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00}, info...)
	logtest.Each(t, func(t *testing.T, on bool) {
		client, decodeCtx := pipeCtx(t, models.MODE_TEST)
		rs := &mysql.BinaryProtocolResultSet{
			ColumnCount:   0,
			FinalResponse: &mysql.GenericResponse{Type: "OK", Data: final},
		}
		logger, logs := logtest.Observe()
		bundle := &mysql.PacketBundle{Header: &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1, SequenceID: 1}, Type: "BinaryProtocolResultSet"}, Message: rs}
		if _, err := EncodeToBinary(context.Background(), logger, bundle, client, decodeCtx); err != nil {
			t.Fatal(err)
		}
		const line = "Writing final response packet for BinaryProtocolResultSet"
		logtest.Check(t, logs, canary, on, line)
		// With the switch on, in hex, as it was.
		if got := logs.FilterMessage(line).All()[0].ContextMap()["final_response_hex"]; on && got != hex.EncodeToString(final) {
			t.Fatalf("with the switch on, the line logs the packet as %v, want its hex", got)
		}
	})
}

// A command this decoder does not know is logged without its payload.
func TestDecodePayload_AnUnknownPacketIsLoggedWithoutItsPayload(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		client, decodeCtx := pipeCtx(t, models.MODE_TEST)
		decodeCtx.LastOp.Store(client, mysql.OK)
		payload := append([]byte{0x7a}, canary...)
		packet := append([]byte{byte(len(payload)), 0, 0, 0}, payload...)
		logger, logs := logtest.Observe()
		if _, err := DecodePayload(context.Background(), logger, packet, client, decodeCtx); err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on, "Unknown packet type")
	})
}
