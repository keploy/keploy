package mysqlparser

import (
	"bytes"
	"encoding/binary"
	"errors"
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

	// if packet.CapabilityFlags&0x00080000 != 0 {
	// 	idx := bytes.IndexByte(data, 0x00)
	// 	if idx == -1 {
	// 		return nil, errors.New("malformed handshake response packet: missing null terminator for AuthPluginName")
	// 	}
	// 	packet.AuthPluginName = string(data[:idx])
	// }

	return packet, nil
}
