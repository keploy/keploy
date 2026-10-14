package wire

import (
	"bytes"
	"context"
	"net"
	"testing"

	connection "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// A decoded HandshakeResponse41 keeps the length of its auth response and
// nothing of it: the response is the client's password in clear under
// mysql_clear_password, and a scramble of it otherwise, and nothing reads it.
// The bytes it was decoded from, what the client sent, are left as they were.
func TestADecodedHandshakeResponseKeepsNoCredential(t *testing.T) {
	secret := []byte("Tr0ub4dor&3-recorded\x00")
	payload, err := connection.EncodeHandshakeResponse41(context.Background(), zap.NewNop(), &mysql.HandshakeResponse41Packet{
		CapabilityFlags: mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION,
		MaxPacketSize:   1 << 24,
		CharacterSet:    0x21,
		Username:        "appuser",
		AuthResponse:    secret,
		AuthPluginName:  "mysql_clear_password",
	})
	if err != nil {
		t.Fatal(err)
	}
	packet := append([]byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 1}, payload...)
	sent := append([]byte(nil), packet...)

	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = peer.Close() })
	decodeCtx := &DecodeContext{Mode: models.MODE_RECORD, LastOp: NewLastOpMap(), ServerGreetings: NewGreetings()}
	decodeCtx.LastOp.Store(client, mysql.HandshakeV10)
	decodeCtx.ServerGreetings.Store(client, &mysql.HandshakeV10Packet{CapabilityFlags: mysql.CLIENT_PROTOCOL_41 | mysql.CLIENT_PLUGIN_AUTH | mysql.CLIENT_SECURE_CONNECTION})

	pkt, err := DecodePayload(context.Background(), zap.NewNop(), packet, client, decodeCtx)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	hr, ok := pkt.Message.(*mysql.HandshakeResponse41Packet)
	if !ok {
		t.Fatalf("decoded a %T; want a HandshakeResponse41", pkt.Message)
	}
	if !bytes.Equal(hr.AuthResponse, make([]byte, len(secret))) {
		t.Fatalf("the decoded auth response is %q; want its length (%d) in zero bytes", hr.AuthResponse, len(secret))
	}
	if hr.Username != "appuser" || hr.AuthPluginName != "mysql_clear_password" {
		t.Fatalf("decoded user %q, plugin %q; want what replay matches on kept", hr.Username, hr.AuthPluginName)
	}
	if !bytes.Equal(packet, sent) {
		t.Fatal("decoding changed the bytes the client sent")
	}
}
