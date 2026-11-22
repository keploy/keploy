package wire

import (
	"context"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// The reading a released keploy made of a COM_QUERY with query attributes (no
// attribute names) is kept at replay only, to match what it recorded; a
// recording never carries it.
func TestDecodePayload_AnOlderKeploysReadingOfAQueryIsKeptAtReplayOnly(t *testing.T) {
	const tp = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	p := []byte{mysql.COM_QUERY, 0x01, 0x01, 0x00, 0x01, byte(mysql.FieldTypeString), 0x00, byte(len("traceparent"))}
	p = append(p, "traceparent"...)
	p = append(append(p, byte(len(tp))), tp...)
	p = append(p, "SELECT 1 AS v"...)
	packet := append([]byte{byte(len(p)), byte(len(p) >> 8), byte(len(p) >> 16), 0}, p...)
	for mode, want := range map[models.Mode]bool{models.MODE_RECORD: false, models.MODE_TEST: true} {
		client, peer := net.Pipe()
		decodeCtx := &DecodeContext{
			Mode: mode, LastOp: NewLastOpMap(), ServerGreetings: NewGreetings(),
			ClientCapabilities: mysql.CLIENT_QUERY_ATTRIBUTES | mysql.CLIENT_PROTOCOL_41,
		}
		decodeCtx.ServerGreetings.Store(client, &mysql.HandshakeV10Packet{CapabilityFlags: mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_QUERY_ATTRIBUTES})
		decodeCtx.LastOp.Store(client, RESET)
		b, err := DecodePayload(context.Background(), zap.NewNop(), packet, client, decodeCtx)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		got := b.Message.(*mysql.QueryPacket)
		if got.Query != "SELECT 1 AS v" || len(got.Parameters) != 1 || got.Parameters[0].Name != "traceparent" {
			t.Fatalf("%s: decoded %+v", mode, got)
		}
		if (got.LegacyQuery != "") != want || (got.LegacyParameters != nil) != want {
			t.Errorf("%s: the older keploy's reading is %q %+v; want it kept: %v", mode, got.LegacyQuery, got.LegacyParameters, want)
		}
		_ = client.Close()
		_ = peer.Close()
	}
}
