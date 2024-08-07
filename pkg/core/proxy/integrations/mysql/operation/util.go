//go:build linux

package operation

import (
	"context"
	"fmt"
	"net"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
)

const RESET = 0x00

type DecodeContext struct {
	Mode               models.Mode
	LastOp             *LastOperation
	PreparedStatements map[uint32]*mysql.StmtPrepareOkPacket
	ServerGreetings    *ServerGreetings
	PluginName         string
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
		return "", fmt.Errorf("invalid caching_sha2_password mechanism")
	}
}

func StringToCachingSha2PasswordMechanism(data string) (mysql.CachingSha2Password, error) {
	switch data {
	case "PerformFullAuthentication":
		return mysql.PerformFullAuthentication, nil
	case "FastAuthSuccess":
		return mysql.FastAuthSuccess, nil
	default:
		return 0, fmt.Errorf("invalid caching_sha2_password mechanism")
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
	case mysql.CommandStatusToString(mysql.COM_STMT_CLOSE), mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA):
		return true
	default:
		return false
	}
}

// PrintByteArray is only for debugging purpose
func PrintByteArray(name string, b []byte) {
	fmt.Printf("%s:\n", name)
	var i = 1
	for _, byte := range b {
		fmt.Printf(" %02x", byte)
		i++
		if i%16 == 0 {
			fmt.Println()
		}
	}
	fmt.Println()
}
