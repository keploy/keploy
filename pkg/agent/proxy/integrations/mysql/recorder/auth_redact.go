package recorder

import (
	intgUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// clientAuthData is the recorded form of a packet of authentication data the
// client sends after its HandshakeResponse41, of type typ: an
// AuthSwitchResponse, or the password of a caching_sha2_password full
// authentication (in clear over TLS, RSA-encrypted without it). It keeps the
// packet's header and, of its payload, only the length, as zero bytes,
// base64 encoded.
//
// That payload is the client's credential, or derived from it: the password
// in clear under mysql_clear_password (PAM, LDAP, cloud IAM tokens) and in a
// full authentication over TLS, and otherwise a scramble of it that the
// recorded greeting's salt opens to a dictionary attack. A recording is user
// data, shared and committed with the test sets, so the recorder keeps none
// of it. Replay needs none of it either: it checks such a packet by its type
// and its sequence id only (replayer/conn.go), and answers with the recorded
// verdict whatever the client sent. Its length is kept so that the recorded
// header and payload still agree.
//
// The bytes the client sent are forwarded to the server as they are: this is
// only what is recorded of them.
func clientAuthData(pkt mysql.Packet, typ string) mysql.Request {
	header := pkt.Header
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &header, Type: typ},
		Message: intgUtils.EncodeBase64(make([]byte, len(pkt.Payload))),
	}}
}
