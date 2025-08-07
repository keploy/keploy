//go:build linux

package conn

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_handshake_response.html
//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_ssl_request.html

func DecodeHandshakeResponse(_ context.Context, logger *zap.Logger, data []byte) (interface{}, error) {

	if len(data) < 32 {
		return nil, errors.New("handshake response packet too short")
	}

	origData := data

	packet := &mysql.HandshakeResponse41Packet{}

	packet.CapabilityFlags = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	if packet.CapabilityFlags&mysql.CLIENT_PROTOCOL_41 == 0 {
		return nil, errors.New("CLIENT_PROTOCOL_41 compatible client is required")
	}

	packet.MaxPacketSize = binary.LittleEndian.Uint32(data[:4])
	data = data[4:]

	packet.CharacterSet = data[0]
	data = data[1:]

	copy(packet.Filler[:], data[:23])
	data = data[23:]

	// Check if it is a SSL Request Packet
	if len(origData) == (4 + 4 + 1 + 23) {
		if packet.CapabilityFlags&mysql.CLIENT_SSL != 0 {
			logger.Debug("Client requested SSL connection")
			return &mysql.SSLRequestPacket{
				CapabilityFlags: packet.CapabilityFlags,
				MaxPacketSize:   packet.MaxPacketSize,
				CharacterSet:    packet.CharacterSet,
				Filler:          packet.Filler,
			}, nil
		}
	}

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

func EncodeHandshakeResponse41(_ context.Context, _ *zap.Logger, packet *mysql.HandshakeResponse41Packet) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write Capability Flags
	if err := binary.Write(buf, binary.LittleEndian, packet.CapabilityFlags); err != nil {
		return nil, fmt.Errorf("failed to write CapabilityFlags for HandshakeResponse41Packet: %w", err)
	}

	// Write Max Packet Size
	if err := binary.Write(buf, binary.LittleEndian, packet.MaxPacketSize); err != nil {
		return nil, fmt.Errorf("failed to write MaxPacketSize for HandshakeResponse41Packet: %w", err)
	}

	// Write Character Set
	if err := buf.WriteByte(packet.CharacterSet); err != nil {
		return nil, fmt.Errorf("failed to write CharacterSet for HandshakeResponse41Packet: %w", err)
	}

	// Write Filler
	if _, err := buf.Write(packet.Filler[:]); err != nil {
		return nil, fmt.Errorf("failed to write Filler for HandshakeResponse41Packet: %w", err)
	}

	// Write Username
	if _, err := buf.WriteString(packet.Username); err != nil {
		return nil, fmt.Errorf("failed to write Username for HandshakeResponse41Packet: %w", err)
	}
	if err := buf.WriteByte(0x00); err != nil {
		return nil, fmt.Errorf("failed to write null terminator for Username for HandshakeResponse41Packet: %w", err)
	}

	// Write Auth Response
	if packet.CapabilityFlags&mysql.CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA != 0 {
		if err := buf.WriteByte(byte(len(packet.AuthResponse))); err != nil {
			return nil, fmt.Errorf("failed to write length of AuthResponse for HandshakeResponse41Packet: %w", err)
		}
		if _, err := buf.Write(packet.AuthResponse); err != nil {
			return nil, fmt.Errorf("failed to write AuthResponse for HandshakeResponse41Packet: %w", err)
		}
	} else {
		if err := buf.WriteByte(byte(len(packet.AuthResponse))); err != nil {
			return nil, fmt.Errorf("failed to write length of AuthResponse for HandshakeResponse41Packet: %w", err)
		}
		if _, err := buf.Write(packet.AuthResponse); err != nil {
			return nil, fmt.Errorf("failed to write AuthResponse for HandshakeResponse41Packet: %w", err)
		}
	}

	// Write Database
	if packet.CapabilityFlags&mysql.CLIENT_CONNECT_WITH_DB != 0 {
		if _, err := buf.WriteString(packet.Database); err != nil {
			return nil, fmt.Errorf("failed to write Database for HandshakeResponse41Packet: %w", err)
		}
		if err := buf.WriteByte(0x00); err != nil {
			return nil, fmt.Errorf("failed to write null terminator for Database for HandshakeResponse41Packet: %w", err)
		}
	}

	// Write Auth Plugin Name
	if packet.CapabilityFlags&mysql.CLIENT_PLUGIN_AUTH != 0 {
		if _, err := buf.WriteString(packet.AuthPluginName); err != nil {
			return nil, fmt.Errorf("failed to write AuthPluginName for HandshakeResponse41Packet: %w", err)
		}
		if err := buf.WriteByte(0x00); err != nil {
			return nil, fmt.Errorf("failed to write null terminator for AuthPluginName for HandshakeResponse41Packet: %w", err)
		}
	}

	// Write Connection Attributes
	if packet.CapabilityFlags&mysql.CLIENT_CONNECT_ATTRS != 0 {
		totalLength := 0
		for key, value := range packet.ConnectionAttributes {
			totalLength += len(key) + len(value) + 2 // 2 bytes for length-encoded integer prefixes
		}

		if err := utils.WriteLengthEncodedInteger(buf, uint64(totalLength)); err != nil {
			return nil, fmt.Errorf("failed to write total length of ConnectionAttributes for HandshakeResponse41Packet: %w", err)
		}

		for key, value := range packet.ConnectionAttributes {
			if err := utils.WriteLengthEncodedString(buf, key); err != nil {
				return nil, fmt.Errorf("failed to write ConnectionAttribute key for HandshakeResponse41Packet: %w", err)
			}
			if err := utils.WriteLengthEncodedString(buf, value); err != nil {
				return nil, fmt.Errorf("failed to write ConnectionAttribute value for HandshakeResponse41Packet: %w", err)
			}
		}
	}
	// Write Zstd Compression Level
	if packet.CapabilityFlags&mysql.CLIENT_ZSTD_COMPRESSION_ALGORITHM != 0 {
		if err := buf.WriteByte(packet.ZstdCompressionLevel); err != nil {
			return nil, fmt.Errorf("failed to write ZstdCompressionLevel for HandshakeResponse41Packet: %w", err)
		}
	}

	return buf.Bytes(), nil
}

// DecodeComQuery parses a COM_QUERY packet. If the CLIENT_QUERY_ATTRIBUTES capability
// is set on the connection, it parses and skips the query attributes section
// before returning the SQL query.
// The input `data` should be the payload of the COM_QUERY packet, with the
// initial command byte (0x03) already removed.
// `capabilityFlags` should be the client's capabilities from the handshake.
func DecodeComQuery(data []byte, capabilityFlags uint32) (string, error) {
	// If the flag is not set, the entire payload is the query.
	if capabilityFlags&mysql.CLIENT_QUERY_ATTRIBUTES == 0 {
		return string(data), nil
	}

	// --- Attributes are present, parse and skip them ---
	// Reference: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_query.html

	// 1. Parameter Count (lenenc)
	paramCount, isNull, n := utils.ReadLengthEncodedInteger(data)
	if isNull || n == 0 {
		return "", errors.New("COM_QUERY: failed to read parameter_count")
	}
	data = data[n:]

	// 2. Parameter Set Count (lenenc)
	// The documentation states this is currently always 1. We just need to parse it.
	_, isNull, n = utils.ReadLengthEncodedInteger(data)
	if isNull || n == 0 {
		return "", errors.New("COM_QUERY: failed to read parameter_set_count")
	}
	data = data[n:]

	if paramCount > 0 {
		// 3. NULL-bitmap, length = (paramCount + 7) / 8
		nullBitmapLen := (int(paramCount) + 7) / 8
		if len(data) < nullBitmapLen {
			return "", fmt.Errorf("COM_QUERY: packet too short for null_bitmap, needs %d bytes but have %d", nullBitmapLen, len(data))
		}
		data = data[nullBitmapLen:]

		// 4. new_params_bind_flag (1 byte)
		if len(data) < 1 {
			return "", errors.New("COM_QUERY: packet too short for new_params_bind_flag")
		}
		newParamsBindFlag := data[0]
		data = data[1:]

		// 5. Parameter types and names (if new_params_bind_flag is 1)
		if newParamsBindFlag == 1 {
			for i := uint64(0); i < paramCount; i++ {
				// param_type_and_flag (2 bytes)
				if len(data) < 2 {
					return "", fmt.Errorf("COM_QUERY: packet too short for parameter type on param #%d", i)
				}
				data = data[2:]

				// parameter name (string<lenenc>)
				_, _, n, err := utils.ReadLengthEncodedString(data)
				if err != nil {
					return "", fmt.Errorf("COM_QUERY: error reading parameter name on param #%d: %w", i, err)
				}
				data = data[n:]
			}
		}
	}

	// The remainder of the packet is the SQL query.
	return string(data), nil
}
