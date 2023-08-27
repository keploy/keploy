package mysqlparser

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

type HandshakeV10Packet struct {
	ProtocolVersion uint8  `yaml:"protocol_version"`
	ServerVersion   string `yaml:"server_version"`
	ConnectionID    uint32 `yaml:"connection_id"`
	AuthPluginData  []byte `yaml:"auth_plugin_data"`
	CapabilityFlags uint32 `yaml:"capability_flags"`
	CharacterSet    uint8  `yaml:"character_set"`
	StatusFlags     uint16 `yaml:"status_flags"`
	AuthPluginName  string `yaml:"auth_plugin_name"`
}

func decodeMySQLHandshakeV10(data []byte) (*HandshakeV10Packet, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("handshake packet too short")
	}

	packet := &HandshakeV10Packet{}
	packet.ProtocolVersion = data[0]

	idx := bytes.IndexByte(data[1:], 0x00)
	if idx == -1 {
		return nil, fmt.Errorf("malformed handshake packet: missing null terminator for ServerVersion")
	}
	packet.ServerVersion = string(data[1 : 1+idx])
	data = data[1+idx+1:]

	packet.ConnectionID = binary.LittleEndian.Uint32(data)
	data = data[4:]

	packet.AuthPluginData = data[:8]
	data = data[8:]

	data = data[1:] // Filler

	if len(data) < 4 {
		return nil, fmt.Errorf("handshake packet too short")
	}
	packet.CapabilityFlags = binary.LittleEndian.Uint32(data)
	data = data[4:]

	packet.CharacterSet = data[0]
	data = data[1:]

	packet.StatusFlags = binary.LittleEndian.Uint16(data)
	data = data[2:]

	if packet.CapabilityFlags&0x800000 != 0 {
		authPluginDataLen := int(data[0])
		if authPluginDataLen > 8 {
			data = data[1:]
			packet.AuthPluginData = append(packet.AuthPluginData, data[:authPluginDataLen-8]...)
			data = data[authPluginDataLen-8:]
		} else {
			data = data[1:]
		}
	}

	data = data[10:] // Reserved 10 bytes

	idx = bytes.IndexByte(data, 0x00)
	if idx == -1 {
		return nil, fmt.Errorf("malformed handshake packet: missing null terminator for AuthPluginName")
	}
	packet.AuthPluginName = string(data[:idx])

	return packet, nil
}
