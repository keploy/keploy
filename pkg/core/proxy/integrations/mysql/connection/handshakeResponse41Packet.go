//go:build linux

package connection

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_handshake_response.html

func DecodeHandshakeResponse41(_ context.Context, _ *zap.Logger, data []byte) (*mysql.HandshakeResponse41Packet, error) {
	if len(data) < 32 {
		return nil, errors.New("handshake response packet too short")
	}

	packet := &mysql.HandshakeResponse41Packet{}

	packet.CapabilityFlags = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	packet.MaxPacketSize = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	packet.CharacterSet = data[0]
	data = data[1:]

	copy(packet.Filler[:], data[:23])
	data = data[23:]

	idx := bytes.IndexByte(data, 0x00)
	if idx == -1 {
		return nil, errors.New("malformed handshake response packet: missing null terminator for Username")
	}

	packet.Username = string(data[:idx])
	data = data[idx+1:]

	if packet.CapabilityFlags&mysql.CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA != 0 {
		length := int(data[0])
		data = data[1:]

		if length > 0 {
			if len(data) < length {
				return nil, errors.New("handshake response packet too short for auth data")
			}
			packet.AuthResponse = data[:length]
			data = data[length:]
		}
	} else {
		authLen := int(data[0])
		data = data[2:]
		packet.AuthResponse = data[:authLen]
	}

	if packet.CapabilityFlags&mysql.CLIENT_CONNECT_WITH_DB != 0 {
		idx = bytes.IndexByte(data, 0x00)
		if idx != -1 {
			packet.Database = string(data[:idx])
			data = data[idx+1:]
		}
	}

	if packet.CapabilityFlags&mysql.CLIENT_PLUGIN_AUTH != 0 {
		idx = bytes.IndexByte(data, 0x00)
		if idx == -1 {
			return nil, errors.New("malformed handshake response packet: missing null terminator for AuthPluginName")
		}
		packet.AuthPluginName = string(data[:idx])
		data = data[idx+1:]
	}

	if packet.CapabilityFlags&mysql.CLIENT_CONNECT_ATTRS != 0 {
		if len(data) < 4 {
			return nil, errors.New("handshake response packet too short for connection attributes")
		}

		totalLength, isNull, n := utils.ReadLengthEncodedInteger(data)
		if isNull || n == 0 {
			return nil, errors.New("error decoding total length of connection attributes")
		}
		data = data[n:]

		attributesData := data[:totalLength]
		data = data[totalLength:]

		packet.ConnectionAttributes = make(map[string]string)
		for len(attributesData) > 0 {
			keyLength, isNull, n := utils.ReadLengthEncodedInteger(attributesData)
			if isNull {
				return nil, errors.New("malformed handshake response packet: null length encoded integer for connection attribute key")
			}
			attributesData = attributesData[n:]

			key := string(attributesData[:keyLength])
			attributesData = attributesData[keyLength:]

			valueLength, isNull, n := utils.ReadLengthEncodedInteger(attributesData)
			if isNull {
				return nil, errors.New("malformed handshake response packet: null length encoded integer for connection attribute value")
			}
			attributesData = attributesData[n:]

			value := string(attributesData[:valueLength])
			attributesData = attributesData[valueLength:]

			packet.ConnectionAttributes[key] = value
		}
	}
	if len(data) > 0 {
		if packet.CapabilityFlags&mysql.CLIENT_ZSTD_COMPRESSION_ALGORITHM != 0 {
			if len(data) < 1 {
				return nil, errors.New("handshake response packet too short for ZSTD compression level")
			}
			packet.ZstdCompressionLevel = data[0]
		}
	}

	return packet, nil
}
