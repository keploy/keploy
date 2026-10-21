package wire

import (
	"context"
	"fmt"
	"net"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

const RESET = 0x00

type DecodeContext struct {
	Mode               models.Mode
	LastOp             *LastOperation
	PreparedStatements map[uint32]*mysql.StmtPrepareOkPacket
	ServerGreetings    *ServerGreetings
	ClientCapabilities uint32
	PluginName         string
	UseSSL             bool
	// Capability flags
	ServerCaps         uint32 // negotiated server caps (from HandshakeV10)
	ClientCaps         uint32 // live client's caps (from HandshakeResponse41)
	RecordedClientCaps uint32 // caps from the recorded config mock
	PreferRecordedCaps bool   // if true, prefer RecordedClientCaps over ClientCaps
	// FramingAssumed is set while ClientCaps is assumed rather than read off
	// the client's HandshakeResponse41 or SSLRequest (a connection joined
	// mid-stream, with no SSLRequest of its own captured): the recorder
	// settles whether CLIENT_DEPRECATE_EOF holds from the first packet that
	// tells, and clears it.
	FramingAssumed bool

	// LongDataParams records which parameters of a prepared statement had
	// their value streamed ahead of time with COM_STMT_SEND_LONG_DATA
	// (stmtID -> set of parameter ids).
	//
	// It has to be tracked out of band because the wire gives no other clue:
	// a long-sent parameter is simply ABSENT from the COM_STMT_EXECUTE
	// payload while still being non-NULL in the null bitmap, so a decoder
	// that reads a value for every non-NULL parameter walks off the end of
	// the packet and rejects the command. That is what dropped every
	// streamed-BLOB EXECUTE from the recording (#4262).
	//
	// The server discards long data when the statement executes or resets,
	// so the entry is cleared at both points.
	LongDataParams map[uint32]map[uint16]bool

	// StmtParamTypes holds, per prepared statement (by ID), the types of
	// its placeholders from its last COM_STMT_EXECUTE that sent them
	// (new_params_bind_flag 1): an EXECUTE with the flag 0 sends none, and
	// the server, and the decoder, read its values with these
	// (preparedstmt.DecodeStmtExecute). A COM_STMT_CLOSE forgets them.
	StmtParamTypes map[uint32][]mysql.Parameter

	// StmtIDToQuery maps the connection's prepared statements, by ID, to
	// their queries. Replay sets it when a COM_STMT_PREPARE matches; record
	// sets it when a COM_STMT_PREPARE_OK answers one, and records the query
	// on each command that names the statement (mysql.MetaQuery).
	StmtIDToQuery map[uint32]string
	// Statement ID counter for generating unique statement IDs during replay
	NextStmtID uint32
}

const CLIENT_DEPRECATE_EOF = 0x01000000

// rememberParamTypes notes the types params were sent with, for the
// statement's EXECUTEs that send none (StmtParamTypes). It keeps no value,
// and no name: a flag-0 EXECUTE sends none. The server keeps the types of the
// statement's placeholders only, not of the query attributes sent after them
// (a flag-0 EXECUTE may still carry NULL ones), so neither does this.
func (d *DecodeContext) rememberParamTypes(stmtID uint32, params []mysql.Parameter) {
	if ps := d.PreparedStatements[stmtID]; ps != nil && int(ps.NumParams) < len(params) {
		params = params[:ps.NumParams]
	}
	types := make([]mysql.Parameter, len(params))
	for i, p := range params {
		types[i] = mysql.Parameter{Type: p.Type, Unsigned: p.Unsigned}
	}
	if d.StmtParamTypes == nil {
		d.StmtParamTypes = make(map[uint32][]mysql.Parameter)
	}
	d.StmtParamTypes[stmtID] = types
}

func (d *DecodeContext) effectiveClientCaps() uint32 {
	if d.PreferRecordedCaps && d.RecordedClientCaps != 0 {
		return d.RecordedClientCaps
	}
	return d.ClientCaps
}

func (d *DecodeContext) DeprecateEOF() bool {
	return (d.ServerCaps&CLIENT_DEPRECATE_EOF) != 0 &&
		(d.effectiveClientCaps()&CLIENT_DEPRECATE_EOF) != 0
}

// This map is used to store the last operation that was performed on a connection.
// It helps us to determine the last mysql packet.

type LastOperation struct {
	sync.RWMutex
	operations map[net.Conn]byte
}

func NewLastOpMap() *LastOperation {
	return &LastOperation{
		operations: make(map[net.Conn]byte),
	}
}

func (lo *LastOperation) Load(key net.Conn) (value byte, ok bool) {
	lo.RLock()
	result, ok := lo.operations[key]
	lo.RUnlock()
	return result, ok
}

func (lo *LastOperation) Store(key net.Conn, value byte) {
	lo.Lock()
	lo.operations[key] = value
	lo.Unlock()
}

// This map is used to store the server greetings for each connection.
// It helps us to determine the server version and capabilities.
// Capabilities are helpful in decoding some packets.

type ServerGreetings struct {
	sync.RWMutex
	handshakes map[net.Conn]*mysql.HandshakeV10Packet
}

func NewGreetings() *ServerGreetings {
	return &ServerGreetings{
		handshakes: make(map[net.Conn]*mysql.HandshakeV10Packet),
	}
}

func (sg *ServerGreetings) Load(key net.Conn) (*mysql.HandshakeV10Packet, bool) {
	sg.RLock()
	result, ok := sg.handshakes[key]
	sg.RUnlock()
	return result, ok
}

func (sg *ServerGreetings) Store(key net.Conn, value *mysql.HandshakeV10Packet) {
	sg.Lock()
	sg.handshakes[key] = value
	sg.Unlock()
}

func setPacketInfo(_ context.Context, parsedPacket *mysql.PacketBundle, pkt interface{}, pktType string, clientConn net.Conn, lastOp byte, decodeCtx *DecodeContext) {
	parsedPacket.Header.Type = pktType
	parsedPacket.Message = pkt
	decodeCtx.LastOp.Store(clientConn, lastOp)
}

func GetPluginName(buf interface{}) (string, error) {
	switch v := buf.(type) {
	case *mysql.HandshakeV10Packet:
		return v.AuthPluginName, nil
	case *mysql.AuthSwitchRequestPacket:
		return v.PluginName, nil
	default:
		return "", fmt.Errorf("invalid packet type to get plugin name")
	}
}

func GetCachingSha2PasswordMechanism(data byte) (string, error) {
	switch data {
	case byte(mysql.PerformFullAuthentication):
		return mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication), nil
	case byte(mysql.FastAuthSuccess):
		return mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess), nil
	default:
		einval := fmt.Sprintf("invalid caching_sha2_password mechanism, found:%02x ", data)
		return "", fmt.Errorf("%s", einval)
	}
}

func StringToCachingSha2PasswordMechanism(data string) (mysql.CachingSha2Password, error) {
	switch data {
	case "PerformFullAuthentication":
		return mysql.PerformFullAuthentication, nil
	case "FastAuthSuccess":
		return mysql.FastAuthSuccess, nil
	default:
		einval := fmt.Sprintf("invalid caching_sha2_password mechanism, found:%s ", data)
		return 0, fmt.Errorf("%s", einval)
	}
}

func IsGenericResponsePkt(packet *mysql.PacketBundle) bool {
	if packet == nil {
		return false
	}
	switch packet.Message.(type) {
	case *mysql.OKPacket, *mysql.ERRPacket, *mysql.EOFPacket:
		return true
	default:
		return false
	}
}

func IsNoResponseCommand(command string) bool {
	switch command {
	// COM_QUIT is answered only by the server closing the socket. Leaving it
	// out made the async recorder queue it as a response-bearing command,
	// where it is never paired and, worse, keeps the queue non-empty — which
	// suppresses the heldResp guard for any genuinely orphaned response that
	// follows it.
	case mysql.CommandStatusToString(mysql.COM_STMT_CLOSE), mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA), mysql.CommandStatusToString(mysql.COM_QUIT):
		return true
	default:
		return false
	}
}
