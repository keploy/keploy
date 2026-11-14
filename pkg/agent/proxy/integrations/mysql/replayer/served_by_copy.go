package replayer

import (
	"encoding/json"
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// ServedByCopy says which MySQL mocks the replay serves by copy
// (syncMock.ServedByCopy), so a recording that lost one keeps every test
// case that holds another:
//
//   - COM_QUIT needs no mock: the command phase ends the connection on it
//     before it reads the store (matchCommandWith), key "".
//   - A handshake (its "config" mock) is served from any recorded handshake
//     the client's answer matches (simulateInitialHandshake): the greeting
//     is the first handshake recorded for the dialled server, and the rest
//     the first whose SSLRequest and HandshakeResponse41 match the client's
//     (matchSSLRequest, matchHanshakeResponse41). Its key is the dialled
//     server and every packet as the replay compares or serves it, without
//     what a connection varies: the greeting's connection id and scramble,
//     an auth switch's scramble, the client's auth response and connection
//     attributes, the payload length of what the client sent, the raw
//     payloads. What the client sends after its HandshakeResponse41 is its
//     credential, which no step compares: its type alone is kept.
//
// Every other MySQL mock is needed in itself: ok is false.
func ServedByCopy(mock *models.Mock) (key string, ok bool) {
	if mock == nil || mock.Kind != models.MySQL {
		return "", false
	}
	if mock.Spec.Metadata["type"] != "config" {
		reqs := mock.Spec.MySQLRequests
		if len(reqs) == 1 && reqs[0].Header != nil && reqs[0].Header.Type == mysql.CommandStatusToString(mysql.COM_QUIT) {
			return "", true
		}
		return "", false
	}
	var b strings.Builder
	b.WriteString("handshake|")
	b.WriteString(mock.Spec.Metadata["destAddr"])
	for _, r := range mock.Spec.MySQLRequests {
		b.WriteString("|>")
		if !writeRequestKey(&b, r.PacketBundle) {
			return "", false
		}
	}
	for _, r := range mock.Spec.MySQLResponses {
		b.WriteString("|<")
		if !writeResponseKey(&b, r.PacketBundle) {
			return "", false
		}
	}
	return b.String(), true
}

// writeRequestKey writes what the replay compares of p, a packet the client
// sent in a handshake (ServedByCopy): its type, and the fields of an
// SSLRequest or a HandshakeResponse41 but the auth response and the
// connection attributes. False when p cannot be read.
func writeRequestKey(b *strings.Builder, p mysql.PacketBundle) bool {
	if p.Header == nil {
		return false
	}
	fmt.Fprintf(b, "%q %T", p.Header.Type, p.Message)
	var v any
	switch msg := p.Message.(type) {
	case *mysql.SSLRequestPacket:
		v = msg
	case *mysql.HandshakeResponse41Packet:
		if msg == nil {
			return false
		}
		c := *msg
		c.AuthResponse, c.ConnectionAttributes = nil, nil
		v = &c
	default:
		return true
	}
	return writeJSON(b, v)
}

// writeResponseKey writes what the replay serves of p, a packet the server
// sent in a handshake (ServedByCopy): its header and its message, but the
// greeting's connection id and scramble and an auth switch's scramble.
// False when p cannot be read.
func writeResponseKey(b *strings.Builder, p mysql.PacketBundle) bool {
	if p.Header == nil {
		return false
	}
	fmt.Fprintf(b, "%q %T", p.Header.Type, p.Message)
	if p.Header.Header != nil {
		fmt.Fprintf(b, " %d/%d", p.Header.Header.PayloadLength, p.Header.Header.SequenceID)
	}
	v := p.Message
	switch msg := p.Message.(type) {
	case *mysql.HandshakeV10Packet:
		if msg == nil {
			return false
		}
		c := *msg
		c.ConnectionID, c.AuthPluginData = 0, nil
		v = &c
	case *mysql.AuthSwitchRequestPacket:
		if msg == nil {
			return false
		}
		c := *msg
		c.PluginData = ""
		v = &c
	}
	return writeJSON(b, v)
}

// writeJSON writes v as JSON, whose map keys are sorted; false when it
// cannot be written.
func writeJSON(b *strings.Builder, v any) bool {
	j, err := json.Marshal(v)
	if err != nil {
		return false
	}
	b.WriteByte(' ')
	b.Write(j)
	return true
}
