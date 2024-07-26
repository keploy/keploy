//go:build linux

// Package connection provides decoding and encoding of connection phase mysql packets
package connection

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_handshake_v10.html

func DecodeHandshakeV10(_ context.Context, _ *zap.Logger, data []byte) (*mysql.HandshakeV10Packet, error) {

	if len(data) < 4 {
		return nil, fmt.Errorf("handshake packet too short")
	}

	packet := &mysql.HandshakeV10Packet{}
	packet.ProtocolVersion = data[0]

	idx := bytes.IndexByte(data[1:], 0x00)
	if idx == -1 {
		return nil, fmt.Errorf("malformed handshake packet: missing null terminator for ServerVersion")
	}

	packet.ServerVersion = string(data[1 : 1+idx])
	data = data[1+idx+1:]

	if len(data) < 4 {
		return nil, fmt.Errorf("handshake packet too short for ConnectionID")
	}

	packet.ConnectionID = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	if len(data) < 9 { // 8 bytes of AuthPluginData + 1 byte filler
		return nil, fmt.Errorf("handshake packet too short for AuthPluginData")
	}
	packet.AuthPluginData = append([]byte{}, data[:8]...)

	packet.Filler = data[8]

	data = data[9:] // Skip 8 bytes of AuthPluginData and 1 byte filler

	if len(data) < 5 { // Capability flags (2 bytes), character set (1 byte), status flags (2 bytes)
		return nil, fmt.Errorf("handshake packet too short for flags")
	}

	capabilityFlagsLower := binary.LittleEndian.Uint16(data[:2])
	data = data[2:]

	packet.CharacterSet = data[0]
	data = data[1:]

	packet.StatusFlags = binary.LittleEndian.Uint16(data[:2])
	data = data[2:]

	capabilityFlagsUpper := binary.LittleEndian.Uint16(data[:2])
	data = data[2:]

	packet.CapabilityFlags = uint32(capabilityFlagsLower) | uint32(capabilityFlagsUpper)<<16
	var authPluginDataLen int

	packet.CapabilityFlags = uint32(capabilityFlagsLower) | uint32(capabilityFlagsUpper)<<16

	if packet.CapabilityFlags&mysql.CLIENT_PLUGIN_AUTH != 0 {
		authPluginDataLen = int(data[0])
		data = data[1:] // Skip 1 byte AuthPluginDataLen
	} else {
		data = data[1:] // constant 0x00
	}

	data = data[10:] // Skip 10 bytes reserved (all 0s)

	if authPluginDataLen > 8 {
		lenToRead := min(authPluginDataLen-8, len(data))
		packet.AuthPluginData = append(packet.AuthPluginData, data[:lenToRead]...)
		data = data[lenToRead:]
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("handshake packet too short for AuthPluginName")
	}

	if packet.CapabilityFlags&mysql.CLIENT_PLUGIN_AUTH != 0 {
		idx = bytes.IndexByte(data, 0x00)
		if idx == -1 {
			return nil, fmt.Errorf("malformed handshake packet: missing null terminator for AuthPluginName")
		}
		packet.AuthPluginName = string(data[:idx])
	}

	return packet, nil
}
