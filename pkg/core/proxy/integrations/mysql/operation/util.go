//go:build linux

package operation

import (
	"context"
	"fmt"
	"net"
	"sync"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

const RESET = 0x00

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

func (sg *ServerGreetings) store(key net.Conn, value *mysql.HandshakeV10Packet) {
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
