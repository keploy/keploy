package mysql

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

type HandshakeV10Packet struct {
	ProtocolVersion uint8  `json:"protocol_version,omitempty" yaml:"protocol_version,omitempty,flow"`
	ServerVersion   string `json:"server_version,omitempty" yaml:"server_version,omitempty,flow"`
	ConnectionID    uint32 `json:"connection_id,omitempty" yaml:"connection_id,omitempty,flow"`
	AuthPluginData  string `json:"auth_plugin_data,omitempty" yaml:"auth_plugin_data,omitempty,flow"`
	CapabilityFlags uint32 `json:"capability_flags,omitempty" yaml:"capability_flags,omitempty,flow"`
	CharacterSet    uint8  `json:"character_set,omitempty" yaml:"character_set,omitempty,flow"`
	StatusFlags     uint16 `json:"status_flags,omitempty" yaml:"status_flags,omitempty,flow"`
	AuthPluginName  string `json:"auth_plugin_name,omitempty" yaml:"auth_plugin_name,omitempty,flow"`
}

func decodeMySQLHandshakeV10(data []byte) (*HandshakeV10Packet, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("handshake packet too short")
	}
	var authPluginDataBytes []byte
	packet := &HandshakeV10Packet{}
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
	authPluginDataBytes = append([]byte{}, data[:8]...)
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

	if packet.CapabilityFlags&0x800000 != 0 {
		if len(data) < 11 { // AuthPluginDataLen (1 byte) + Reserved (10 bytes)
			return nil, fmt.Errorf("handshake packet too short for AuthPluginDataLen")
		}
		authPluginDataLen := int(data[0])
		data = data[11:] // Skip 1 byte AuthPluginDataLen and 10 bytes reserved

		if authPluginDataLen > 8 {
			lenToRead := min(authPluginDataLen-8, len(data))
			authPluginDataBytes = append(authPluginDataBytes, data[:lenToRead]...)
			data = data[lenToRead:]
		}
	} else {
		data = data[10:] // Skip reserved 10 bytes if CLIENT_PLUGIN_AUTH is not set
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("handshake packet too short for AuthPluginName")
	}

	idx = bytes.IndexByte(data, 0x00)
	if idx == -1 {
		return nil, fmt.Errorf("malformed handshake packet: missing null terminator for AuthPluginName")
	}
	packet.AuthPluginName = string(data[:idx])
	packet.AuthPluginData = base64.StdEncoding.EncodeToString(authPluginDataBytes)
	return packet, nil
}

func encodeHandshakePacket(packet *models.MySQLHandshakeV10Packet) ([]byte, error) {
	buf := new(bytes.Buffer)
	AuthPluginDataValue, _ := base64.StdEncoding.DecodeString(packet.AuthPluginData)
	// Protocol version
	buf.WriteByte(packet.ProtocolVersion)

	// Server version
	buf.WriteString(packet.ServerVersion)
	buf.WriteByte(0x00) // Null terminator

	// Connection ID
	if err := binary.Write(buf, binary.LittleEndian, packet.ConnectionID); err != nil {
		return nil, err
	}

	// Auth-plugin-data-part-1 (first 8 bytes)
	if len(AuthPluginDataValue) < 8 {
		return nil, errors.New("auth plugin data too short")
	}
	buf.Write(AuthPluginDataValue[:8])

	// Filler
	buf.WriteByte(0x00)

	// Capability flags
	if err := binary.Write(buf, binary.LittleEndian, uint16(packet.CapabilityFlags)); err != nil {
		return nil, err
	}
	// binary.Write(buf, binary.LittleEndian, uint16(packet.CapabilityFlags))

	// Character set
	buf.WriteByte(packet.CharacterSet)

	// Status flags
	if err := binary.Write(buf, binary.LittleEndian, packet.StatusFlags); err != nil {
		return nil, err
	}
	if err := binary.Write(buf, binary.LittleEndian, uint16(packet.CapabilityFlags>>16)); err != nil {
		return nil, err
	}

	// Length of auth-plugin-data
	if packet.CapabilityFlags&0x800000 != 0 && len(AuthPluginDataValue) >= 21 {
		buf.WriteByte(byte(len(AuthPluginDataValue))) // Length of entire auth plugin data
	} else {
		buf.WriteByte(0x00)
	}
	// Reserved (10 zero bytes)
	buf.Write(make([]byte, 10))

	// Auth-plugin-data-part-2 (remaining auth data)
	if packet.CapabilityFlags&0x800000 != 0 && len(AuthPluginDataValue) >= 21 {

		buf.Write(AuthPluginDataValue[8:]) // Write all remaining bytes of auth plugin data
	}
	// Auth-plugin name
	if packet.CapabilityFlags&0x800000 != 0 {
		buf.WriteString(packet.AuthPluginName)
		buf.WriteByte(0x00) // Null terminator
	}

	return buf.Bytes(), nil
}
