package mysqlparser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"go.keploy.io/server/pkg/models"
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

	packet.ConnectionID = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	packet.AuthPluginData = data[:8]
	data = data[8:]

	data = data[1:] // Skip filler

	if len(data) < 5 { // Ensuring enough data for capability flags and character set
		return nil, fmt.Errorf("handshake packet too short")
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
	if packet.CapabilityFlags&0x800000 != 0 {
		authPluginDataLen = int(data[0])
		data = data[1:]
	} else {
		data = data[1:] // Skip the 0x00 byte if CLIENT_PLUGIN_AUTH is not set
	}

	if authPluginDataLen > 8 {
		lenToRead := min(authPluginDataLen-8, len(data))
		packet.AuthPluginData = append(packet.AuthPluginData, data[:lenToRead]...)
		data = data[lenToRead:]
	}

	data = data[10:] // Skip reserved 10 bytes

	if len(data) == 0 {
		return nil, fmt.Errorf("handshake packet too short for AuthPluginName")
	}

	idx = bytes.IndexByte(data, 0x00)
	if idx == -1 {
		return nil, fmt.Errorf("malformed handshake packet: missing null terminator for AuthPluginName")
	}
	packet.AuthPluginName = string(data[:idx])

	return packet, nil
}

// Helper function to calculate minimum of two integers
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func encodeHandshakePacket(packet *models.MySQLHandshakeV10Packet) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Protocol version
	buf.WriteByte(packet.ProtocolVersion)

	// Server version
	buf.WriteString(packet.ServerVersion)
	buf.WriteByte(0x00) // Null terminator

	// Connection ID
	binary.Write(buf, binary.LittleEndian, packet.ConnectionID)

	// Auth-plugin-data-part-1 (first 8 bytes)
	if len(packet.AuthPluginData) < 8 {
		return nil, errors.New("auth plugin data too short")
	}
	buf.Write(packet.AuthPluginData[:8])

	// Filler
	buf.WriteByte(0x00)

	// Lower 2 bytes of CapabilityFlags
	binary.Write(buf, binary.LittleEndian, uint16(packet.CapabilityFlags))

	// Character set
	buf.WriteByte(packet.CharacterSet)

	// Status flags
	binary.Write(buf, binary.LittleEndian, packet.StatusFlags)

	// Upper 2 bytes of CapabilityFlags
	binary.Write(buf, binary.LittleEndian, uint16(packet.CapabilityFlags>>16))

	// Length of auth-plugin-data (always 0x15 for the current version of the MySQL protocol)
	buf.WriteByte(0x15)

	// Reserved (10 zero bytes)
	buf.Write(make([]byte, 10))

	// Auth-plugin-data-part-2 (remaining auth data, 13 bytes, without the last byte)
	if len(packet.AuthPluginData) < 21 {
		return nil, errors.New("auth plugin data too short")
	}
	buf.Write(packet.AuthPluginData[8:20])

	// Null terminator for auth-plugin-data
	buf.WriteByte(0x00)

	// Auth-plugin name
	buf.WriteString(packet.AuthPluginName)
	buf.WriteByte(0x00) // Null terminator

	return buf.Bytes(), nil
}
