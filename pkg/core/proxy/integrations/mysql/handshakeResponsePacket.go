package mysql

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
)

const (
	CLIENT_PLUGIN_AUTH                = 0x00080000
	CLIENT_CONNECT_WITH_DB            = 0x00000008
	CLIENT_CONNECT_ATTRS              = 0x00100000
	CLIENT_ZSTD_COMPRESSION_ALGORITHM = 0x00010000
)

type HandshakeResponse struct {
	CapabilityFlags      uint32            `json:"capability_flags,omitempty" yaml:"capability_flags,omitempty,flow"`
	MaxPacketSize        uint32            `json:"max_packet_size,omitempty" yaml:"max_packet_size,omitempty,flow"`
	CharacterSet         uint8             `json:"character_set,omitempty" yaml:"character_set,omitempty,flow"`
	Reserved             int               `json:"reserved,omitempty" yaml:"reserved,omitempty,flow"`
	Username             string            `json:"username,omitempty" yaml:"username,omitempty,flow"`
	AuthData             string            `json:"auth_data,omitempty" yaml:"auth_data,omitempty,flow"`
	Database             string            `json:"database,omitempty" yaml:"database,omitempty,flow"`
	AuthPluginName       string            `json:"auth_plugin_name,omitempty" yaml:"auth_plugin_name,omitempty,flow"`
	ConnectAttributes    map[string]string `json:"connect_attributes,omitempty" yaml:"connect_attributes,omitempty,flow"`
	ZstdCompressionLevel byte              `json:"zstdcompressionlevel,omitempty" yaml:"zstdcompressionlevel,omitempty,flow"`
}

func decodeHandshakeResponse(data []byte) (*HandshakeResponse, error) {
	if len(data) < 32 {
		return nil, errors.New("handshake response packet too short")
	}
	packet := &HandshakeResponse{}
	var authDataByte []byte
	var reservedBytes [23]byte
	packet.CapabilityFlags = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	packet.MaxPacketSize = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	packet.CharacterSet = data[0]
	data = data[1:]

	copy(reservedBytes[:], data[:23])
	data = data[23:]

	idx := bytes.IndexByte(data, 0x00)
	if idx == -1 {
		return nil, errors.New("malformed handshake response packet: missing null terminator for Username")
	}
	packet.Username = string(data[:idx])
	data = data[idx+1:]

	if packet.CapabilityFlags&CLIENT_PLUGIN_AUTH != 0 {
		length := int(data[0])
		data = data[1:]

		if length > 0 {
			if len(data) < length {
				return nil, errors.New("handshake response packet too short for auth data")
			}
			authDataByte = data[:length]
			data = data[length:]
		}
	} else {
		idx = bytes.IndexByte(data, 0x00)
		if idx != -1 {
			authDataByte = data[:idx]
			data = data[idx+1:]
		}
	}

	if packet.CapabilityFlags&CLIENT_CONNECT_WITH_DB != 0 {
		idx = bytes.IndexByte(data, 0x00)
		if idx != -1 {
			packet.Database = string(data[:idx])
			data = data[idx+1:]
		}
	}

	if packet.CapabilityFlags&CLIENT_PLUGIN_AUTH != 0 {
		idx = bytes.IndexByte(data, 0x00)
		if idx == -1 {
			return nil, errors.New("malformed handshake response packet: missing null terminator for AuthPluginName")
		}
		packet.AuthPluginName = string(data[:idx])
		data = data[idx+1:]
	}

	if packet.CapabilityFlags&CLIENT_CONNECT_ATTRS != 0 {
		if len(data) < 4 {
			return nil, errors.New("handshake response packet too short for conn attributes")
		}

		totalLength, isNull, n := decodeLengthEncodedInteger(data)
		if isNull || n == 0 {
			return nil, errors.New("error decoding total length of conn attributes")
		}
		data = data[n:]

		attributesData := data[:totalLength]
		data = data[totalLength:]

		packet.ConnectAttributes = make(map[string]string)
		for len(attributesData) > 0 {
			keyLength, isNull, n := decodeLengthEncodedInteger(attributesData)
			if isNull {
				return nil, errors.New("malformed handshake response packet: null length encoded integer for conn attribute key")
			}
			attributesData = attributesData[n:]

			key := string(attributesData[:keyLength])
			attributesData = attributesData[keyLength:]

			valueLength, isNull, n := decodeLengthEncodedInteger(attributesData)
			if isNull {
				return nil, errors.New("malformed handshake response packet: null length encoded integer for conn attribute value")
			}
			attributesData = attributesData[n:]

			value := string(attributesData[:valueLength])
			attributesData = attributesData[valueLength:]

			packet.ConnectAttributes[key] = value
		}
	}
	if len(data) > 0 {
		if packet.CapabilityFlags&CLIENT_ZSTD_COMPRESSION_ALGORITHM != 0 {
			if len(data) < 1 {
				return nil, errors.New("handshake response packet too short for ZSTD compression level")
			}
			packet.ZstdCompressionLevel = data[0]
			data = data[1:]
		}
	}
	packet.AuthData = base64.StdEncoding.EncodeToString(authDataByte)
	packet.Reserved = len(reservedBytes)
	return packet, nil
}
func decodeLengthEncodedInteger(b []byte) (length int, isNull bool, bytesRead int) {
	if len(b) == 0 {
		return 0, true, 0
	}

	switch b[0] {
	case 0xfb:
		return 0, true, 1
	case 0xfc:
		if len(b) < 3 {
			return 0, false, 0
		}
		return int(binary.LittleEndian.Uint16(b[1:3])), false, 3
	case 0xfd:
		if len(b) < 4 {
			return 0, false, 0
		}
		return int(b[1]) | int(b[2])<<8 | int(b[3])<<16, false, 4
	case 0xfe:
		if len(b) < 9 {
			return 0, false, 0
		}
		return int(binary.LittleEndian.Uint64(b[1:9])), false, 9
	default:
		return int(b[0]), false, 1
	}
}
