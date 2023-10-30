package mysqlparser

import (
	"bytes"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/pkg/models"
)

type HandshakeResponse struct {
	CapabilityFlags uint32   `yaml:"capability_flags"`
	MaxPacketSize   uint32   `yaml:"max_packet_size"`
	CharacterSet    uint8    `yaml:"character_set"`
	Reserved        [23]byte `yaml:"reserved"`
	Username        string   `yaml:"username"`
	AuthData        []byte   `yaml:"auth_data"`
	Database        string   `yaml:"database"`
	AuthPluginName  string   `yaml:"auth_plugin_name"`
}

func decodeHandshakeResponse(data []byte) (*HandshakeResponse, error) {
	if len(data) < 32 {
		return nil, errors.New("handshake response packet too short")
	}

	packet := &HandshakeResponse{}

	packet.CapabilityFlags = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	packet.MaxPacketSize = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	packet.CharacterSet = data[0]
	data = data[1:]

	copy(packet.Reserved[:], data[:23])
	data = data[23:]

	idx := bytes.IndexByte(data, 0x00)
	if idx == -1 {
		return nil, errors.New("malformed handshake response packet: missing null terminator for Username")
	}
	packet.Username = string(data[:idx])
	data = data[idx+1:]

	authDataLen := int(data[0])
	data = data[1:]

	if len(data) < authDataLen {
		return nil, errors.New("handshake response packet too short for auth data")
	}
	packet.AuthData = data[:authDataLen]
	data = data[authDataLen:]

	idx = bytes.IndexByte(data, 0x00)
	if idx != -1 {
		packet.Database = string(data[:idx])
		data = data[idx+1:]
	}

	if packet.CapabilityFlags&0x00080000 != 0 {
		idx := bytes.IndexByte(data, 0x00)
		if idx == -1 {
			return nil, errors.New("malformed handshake response packet: missing null terminator for AuthPluginName")
		}
		packet.AuthPluginName = string(data[:idx])
	}

	return packet, nil
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
