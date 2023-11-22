package mysqlparser

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

// common
type PacketHeader struct {
	PacketLength     uint8 `yaml:"packet_length"`
	PacketSequenceID uint8 `yaml:"packet_sequence_id"`
}

type MySQLPacketHeader struct {
	PayloadLength uint32 `yaml:"payload_length"` // MySQL packet payload length
	SequenceID    uint8  `yaml:"sequence_id"`    // MySQL packet sequence ID
}

type MySQLPacket struct {
	Header  MySQLPacketHeader `yaml:"header"`
	Payload []byte            `yaml:"payload"`
}

type ColumnDefinition struct {
	PacketHeader PacketHeader `yaml:"packet_header"`
	Catalog      string       `yaml:"catalog"`
	Schema       string       `yaml:"schema"`
	Table        string       `yaml:"table"`
	OrgTable     string       `yaml:"org_table"`
	Name         string       `yaml:"name"`
	OrgName      string       `yaml:"org_name"`
	NextLength   uint64       `yaml:"next_length"`
	CharacterSet uint16       `yaml:"character_set"`
	ColumnLength uint32       `yaml:"column_length"`
	ColumnType   byte         `yaml:"column_type"`
	Flags        uint16       `yaml:"flags"`
	Decimals     byte         `yaml:"decimals"`
	DefaultValue string       `yaml:"string"`
}

type RowDataPacket struct {
	Data []byte `yaml:"data"`
}

type PluginDetails struct {
	Type    string `yaml:"type"`
	Message string `yaml:"message"`
}

type CapabilityFlags uint32

var handshakePluginName string

func encodeToBinary(packet interface{}, header *models.MySQLPacketHeader, operation string, sequence int) ([]byte, error) {
	var data []byte
	var err error
	var bypassHeader = false
	innerPacket, ok := packet.(*interface{})
	if ok {
		packet = *innerPacket
	}
	switch operation {
	case "MySQLHandshakeV10":
		p, ok := packet.(*models.MySQLHandshakeV10Packet)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for HandshakeV10Packet: expected *HandshakeV10Packet, got %T", packet)
		}
		data, err = encodeHandshakePacket(p)
	case "HANDSHAKE_RESPONSE_OK":
		bypassHeader = true
		p, ok := packet.(*models.MySQLHandshakeResponseOk)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for HandshakeResponse: expected *HandshakeResponse, got %T", packet)
		}
		data, err = encodeHandshakeResponseOk(p)
	case "AUTH_SWITCH_REQUEST":
		p, ok := packet.(*models.AuthSwitchRequestPacket)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for HandshakeV10Packet: expected *HandshakeV10Packet, got %T", packet)
		}
		data, err = encodeAuthSwitchRequest(p)
	case "AUTH_SWITCH_RESPONSE":
		p, ok := packet.(*models.AuthSwitchResponsePacket)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for HandshakeV10Packet: expected *HandshakeV10Packet, got %T", packet)
		}
		data, err = encodeAuthSwitchResponse(p)

	case "MySQLOK":
		p, ok := packet.(*models.MySQLOKPacket)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for HandshakeResponse: expected *HandshakeResponse, got %T", packet)
		}
		data, err = encodeMySQLOK(p, header)
		bypassHeader = true
	case "COM_STMT_PREPARE_OK":
		p, ok := packet.(*models.MySQLStmtPrepareOk)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for HandshakeResponse: expected *HandshakeResponse, got %T", packet)
		}
		data, err = encodeStmtPrepareOk(p)
		bypassHeader = true
	case "RESULT_SET_PACKET":
		p, ok := packet.(*models.MySQLResultSet)
		if !ok {
			return nil, fmt.Errorf("invalid packet for result set")
		}
		data, err = encodeMySQLResultSet(p)
		bypassHeader = true
	default:
		return nil, errors.New("unknown operation type")
	}

	if err != nil {
		return nil, err
	}
	if !bypassHeader {
		header := make([]byte, 4)
		binary.LittleEndian.PutUint32(header, uint32(len(data)))
		header[3] = byte(sequence)
		return append(header, data...), nil
	} else {
		return data, nil
	}
}

func DecodeMySQLPacket(packet MySQLPacket, logger *zap.Logger, destConn net.Conn) (string, MySQLPacketHeader, interface{}, error) {
	data := packet.Payload
	header := packet.Header
	fmt.Println("\n", data, header)
	var packetData interface{}
	var packetType string
	var err error

	if len(data) < 1 {
		return "", MySQLPacketHeader{}, nil, fmt.Errorf("Invalid packet: Payload is empty")
	}

	switch {
	case data[0] == 0x0e: // COM_PING
		packetType = "COM_PING"
		packetData, err = decodeComPing(data)
		lastCommand = 0x0e
	case data[0] == 0x17: // COM_STMT_EXECUTE
		packetType = "COM_STMT_EXECUTE"
		packetData, err = decodeComStmtExecute(data)
		lastCommand = 0x17
	case data[0] == 0x1c: // COM_STMT_FETCH
		packetType = "COM_STMT_FETCH"
		packetData, err = decodeComStmtFetch(data)
		lastCommand = 0x1c
	case data[0] == 0x16: // COM_STMT_PREPARE
		packetType = "COM_STMT_PREPARE"
		packetData, err = decodeComStmtPrepare(data)
		lastCommand = 0x16
	case data[0] == 0x19: // COM_STMT_CLOSE
		if len(data) > 11 {

			packetType = "COM_STMT_CLOSE_WITH_PREPARE"
			packetData, err = decodeComStmtCloseMoreData(data)
			lastCommand = 0x16
		} else {
			packetType = "COM_STMT_CLOSE"
			packetData, err = decodeComStmtClose(data)
			lastCommand = 0x19
		}
	case data[0] == 0x11: // COM_CHANGE_USER
		packetType = "COM_CHANGE_USER"
		packetData, err = decodeComChangeUser(data)
		lastCommand = 0x11

	case lastCommand == 0x03:
		switch {
		case data[0] == 0x00: // OK Packet
			packetType = "MySQLOK"
			packetData, err = decodeMySQLOK(data)
			lastCommand = 0x00 // Reset the last command

		case data[0] == 0xFF: // Error Packet
			packetType = "MySQLErr"
			packetData, err = decodeMySQLErr(data)
			lastCommand = 0x00 // Reset the last command

		case isLengthEncodedInteger(data[0]): // ResultSet Packet
			packetType = "RESULT_SET_PACKET"
			packetData, err = parseResultSet(data)
			lastCommand = 0x00 // Reset the last command

		default:
			packetType = "Unknown"
			packetData = data
			logger.Warn("unknown packet type after COM_QUERY", zap.Int("unknownPacketTypeInt", int(data[0])))
		}
	case data[0] == 0x04: // Result Set Packet
		packetType = "RESULT_SET_PACKET"
		packetData, err = parseResultSet(data)
		lastCommand = 0x04
	case data[0] == 0x0A: // MySQLHandshakeV10
		packetType = "MySQLHandshakeV10"
		packetData, err = decodeMySQLHandshakeV10(data)
		handshakePacket, _ := packetData.(*HandshakeV10Packet)
		handshakePluginName = handshakePacket.AuthPluginName
		lastCommand = 0x0A
	case data[0] == 0x03: // MySQLQuery
		packetType = "MySQLQuery"
		packetData, err = decodeMySQLQuery(data)
		lastCommand = 0x03
	case data[0] == 0x00: // MySQLOK or COM_STMT_PREPARE_OK
		if lastCommand == 0x16 {
			packetType = "COM_STMT_PREPARE_OK"
			packetData, err = decodeComStmtPrepareOk(data)
		} else {
			packetType = "MySQLOK"
			packetData, err = decodeMySQLOK(data)
		}
		lastCommand = 0x00
	case data[0] == 0xFF: // MySQLErr
		packetType = "MySQLErr"
		packetData, err = decodeMySQLErr(data)
		lastCommand = 0xFF
	case data[0] == 0xFE && len(data) > 1: // Auth Switch Packet
		packetType = "AUTH_SWITCH_REQUEST"
		packetData, err = decodeAuthSwitchRequest(data)
		lastCommand = 0xFE
	case data[0] == 0xFE || expectingAuthSwitchResponse:
		packetType = "AUTH_SWITCH_RESPONSE"
		packetData, err = decodeAuthSwitchResponse(data)
		expectingAuthSwitchResponse = false
	case data[0] == 0xFE: // EOF packet
		packetType = "MySQLEOF"
		packetData, err = decodeMYSQLEOF(data)
		lastCommand = 0xFE
	case data[0] == 0x02: // New packet type
		packetType = "AUTH_MORE_DATA"
		packetData, err = decodeAuthMoreData(data)
		lastCommand = 0x02
	case data[0] == 0x18: // SEND_LONG_DATA Packet
		packetType = "COM_STMT_SEND_LONG_DATA"
		packetData, err = decodeComStmtSendLongData(data)
		lastCommand = 0x18
	case data[0] == 0x1a: // STMT_RESET Packet
		packetType = "COM_STMT_RESET"
		packetData, err = decodeComStmtReset(data)
		lastCommand = 0x1a
	case data[0] == 0x8d || expectingHandshakeResponse: // Handshake Response packet
		packetType = "HANDSHAKE_RESPONSE"
		packetData, err = decodeHandshakeResponse(data)
		lastCommand = 0x8d // This value may differ depending on the handshake response protocol version
	case data[0] == 0x01: // Handshake Response packet
		packetType = "HANDSHAKE_RESPONSE_OK"
		packetData, err = decodeHandshakeResponseOk(data)
	default:
		packetType = "Unknown"
		packetData = data
		logger.Warn("unknown packet type", zap.Int("unknownPacketTypeInt", int(data[0])))
	}

	if err != nil {
		return "", MySQLPacketHeader{}, nil, err
	}
	fmt.Println(packetType+"\n", data, header)
	return packetType, header, packetData, nil
}
func isLengthEncodedInteger(b byte) bool {
	// This is a simplified check. You may need a more robust check based on MySQL protocol.
	return b != 0x00 && b != 0xFF
}

func (p *MySQLPacket) Encode() ([]byte, error) {
	packet := make([]byte, 4)

	binary.LittleEndian.PutUint32(packet[:3], p.Header.PayloadLength)
	packet[3] = p.Header.SequenceID

	// Simplistic interpretation of MySQL's COM_QUERY
	if p.Payload[0] == 0x03 {
		query := string(p.Payload[1:])
		queryObj := map[string]interface{}{
			"command": "COM_QUERY",
			"query":   query,
		}
		queryJson, _ := json.Marshal(queryObj)
		packet = append(packet, queryJson...)
	}

	return packet, nil
}

var lastCommand byte // This is global and will remember the last command

func encodeLengthEncodedInteger(n uint64) []byte {
	var buf []byte

	if n <= 250 {
		buf = append(buf, byte(n))
	} else if n <= 0xffff {
		buf = append(buf, 0xfc, byte(n), byte(n>>8))
	} else if n <= 0xffffff {
		buf = append(buf, 0xfd, byte(n), byte(n>>8), byte(n>>16))
	} else {
		buf = append(buf, 0xfe, byte(n), byte(n>>8), byte(n>>16), byte(n>>24), byte(n>>32), byte(n>>40), byte(n>>48), byte(n>>56))
	}

	return buf
}

func writeLengthEncodedString(buf *bytes.Buffer, s string) {
	length := len(s)
	switch {
	case length <= 250:
		buf.WriteByte(byte(length))
	case length <= 0xFFFF:
		buf.WriteByte(0xFC)
		binary.Write(buf, binary.LittleEndian, uint16(length))
	case length <= 0xFFFFFF:
		buf.WriteByte(0xFD)
		binary.Write(buf, binary.LittleEndian, uint32(length)&0xFFFFFF)
	default:
		buf.WriteByte(0xFE)
		binary.Write(buf, binary.LittleEndian, uint64(length))
	}
	buf.WriteString(s)
}

func writeLengthEncodedInteger(buf *bytes.Buffer, val uint64) {
	switch {
	case val <= 250:
		buf.WriteByte(byte(val))
	case val <= 0xFFFF:
		buf.WriteByte(0xFC)
		binary.Write(buf, binary.LittleEndian, uint16(val))
	case val <= 0xFFFFFF:
		buf.WriteByte(0xFD)
		binary.Write(buf, binary.LittleEndian, uint32(val)&0xFFFFFF)
	default:
		buf.WriteByte(0xFE)
		binary.Write(buf, binary.LittleEndian, val)
	}
}

func writeLengthEncodedIntegers(buf *bytes.Buffer, value uint64) {
	if value <= 250 {
		buf.WriteByte(byte(value))
	} else if value <= 0xffff {
		buf.WriteByte(0xfc)
		buf.WriteByte(byte(value))
		buf.WriteByte(byte(value >> 8))
	} else if value <= 0xffffff {
		buf.WriteByte(0xfd)
		buf.WriteByte(byte(value))
		buf.WriteByte(byte(value >> 8))
		buf.WriteByte(byte(value >> 16))
	} else {
		buf.WriteByte(0xfe)
		binary.Write(buf, binary.LittleEndian, value)
	}
}

func writeLengthEncodedStrings(buf *bytes.Buffer, value string) {
	data := []byte(value)
	length := uint64(len(data))
	writeLengthEncodedIntegers(buf, length)
	buf.Write(data)
}

func readLengthEncodedString(data []byte, offset *int) (string, error) {
	if *offset >= len(data) {
		return "", errors.New("data length is not enough")
	}
	var length int
	firstByte := data[*offset]
	switch {
	case firstByte < 0xfb:
		length = int(firstByte)
		*offset++
	case firstByte == 0xfb:
		*offset++
		return "", nil
	case firstByte == 0xfc:
		if *offset+3 > len(data) {
			return "", errors.New("data length is not enough 1")
		}
		length = int(binary.LittleEndian.Uint16(data[*offset+1 : *offset+3]))
		*offset += 3
	case firstByte == 0xfd:
		if *offset+4 > len(data) {
			return "", errors.New("data length is not enough 2")
		}
		length = int(data[*offset+1]) | int(data[*offset+2])<<8 | int(data[*offset+3])<<16
		*offset += 4
	case firstByte == 0xfe:
		if *offset+9 > len(data) {
			return "", errors.New("data length is not enough 3")
		}
		length = int(binary.LittleEndian.Uint64(data[*offset+1 : *offset+9]))
		*offset += 9
	}
	result := string(data[*offset : *offset+length])
	*offset += length
	return result, nil
}

func ReadLengthEncodedIntegers(data []byte, offset int) (uint64, int) {
	if data[offset] < 0xfb {
		return uint64(data[offset]), offset + 1
	} else if data[offset] == 0xfc {
		return uint64(binary.LittleEndian.Uint16(data[offset+1 : offset+3])), offset + 3
	} else if data[offset] == 0xfd {
		return uint64(data[offset+1]) | uint64(data[offset+2])<<8 | uint64(data[offset+3])<<16, offset + 4
	} else {
		return binary.LittleEndian.Uint64(data[offset+1 : offset+9]), offset + 9
	}
}

func nullTerminatedString(data []byte) (string, int, error) {
	pos := bytes.IndexByte(data, 0)
	if pos == -1 {
		return "", 0, errors.New("null-terminated string not found")
	}
	return string(data[:pos]), pos, nil
}

func readLengthEncodedInteger(b []byte) (uint64, bool, int) {
	if len(b) == 0 {
		return 0, true, 1
	}
	switch b[0] {
	case 0xfb:
		return 0, true, 1
	case 0xfc:
		return uint64(b[1]) | uint64(b[2])<<8, false, 3
	case 0xfd:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, false, 4
	case 0xfe:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16 |
				uint64(b[4])<<24 | uint64(b[5])<<32 | uint64(b[6])<<40 |
				uint64(b[7])<<48 | uint64(b[8])<<56,
			false, 9
	default:
		return uint64(b[0]), false, 1
	}
}

func readLengthEncodedStringUpdated(data []byte) (string, []byte, error) {
	// First, determine the length of the string
	strLength, isNull, bytesRead := readLengthEncodedInteger(data)
	if isNull {
		return "", nil, errors.New("NULL value encountered")
	}

	// Adjust data to point to the next bytes after the integer
	data = data[bytesRead:]

	// Check if we have enough data left to read the string
	if len(data) < int(strLength) {
		return "", nil, errors.New("not enough data to read string")
	}

	// Read the string
	strData := data[:strLength]
	remainingData := data[strLength:]

	// Convert the byte array to a string
	str := string(strData)

	return str, remainingData, nil
}

func readUint24(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}

func readLengthEncodedIntegers(b []byte) (uint64, int) {
	// Check the first byte
	switch b[0] {
	case 0xfb:
		// 0xfb represents NULL
		return 0, 1
	case 0xfc:
		// 0xfc means the next 2 bytes are the integer
		return uint64(binary.LittleEndian.Uint16(b[1:])), 3
	case 0xfd:
		// 0xfd means the next 3 bytes are the integer
		return uint64(binary.LittleEndian.Uint32(append(b[1:4], 0))), 4
	case 0xfe:
		// 0xfe means the next 8 bytes are the integer
		return binary.LittleEndian.Uint64(b[1:]), 9
	default:
		// If the first byte is less than 0xfb, it is the integer itself
		return uint64(b[0]), 1
	}
}

func readLengthEncodedStrings(b []byte) (string, int) {
	length, n := readLengthEncodedIntegers(b)
	return string(b[n : n+int(length)]), n + int(length)
}

func (packet *HandshakeV10Packet) ShouldUseSSL() bool {
	return (packet.CapabilityFlags & models.CLIENT_SSL) != 0
}

func (packet *HandshakeV10Packet) GetAuthMethod() string {
	// It will return the auth method
	return packet.AuthPluginName
}

func Uint24(data []byte) uint32 {
	return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16
}

func readLengthEncodedIntegerOff(data []byte, offset *int) (uint64, error) {
	if *offset >= len(data) {
		return 0, errors.New("data length is not enough")
	}
	var length int
	firstByte := data[*offset]
	switch {
	case firstByte < 0xfb:
		length = int(firstByte)
		*offset++
	case firstByte == 0xfb:
		*offset++
		return 0, nil
	case firstByte == 0xfc:
		if *offset+3 > len(data) {
			return 0, errors.New("data length is not enough 1")
		}
		length = int(binary.LittleEndian.Uint16(data[*offset+1 : *offset+3]))
		*offset += 3
	case firstByte == 0xfd:
		if *offset+4 > len(data) {
			return 0, errors.New("data length is not enough 2")
		}
		length = int(data[*offset+1]) | int(data[*offset+2])<<8 | int(data[*offset+3])<<16
		*offset += 4
	case firstByte == 0xfe:
		if *offset+9 > len(data) {
			return 0, errors.New("data length is not enough 3")
		}
		length = int(binary.LittleEndian.Uint64(data[*offset+1 : *offset+9]))
		*offset += 9
	}
	result := uint64(length)
	return result, nil
}

func readLengthEncodedStringOff(data []byte, offset *int) (string, error) {
	if *offset >= len(data) {
		return "", errors.New("data length is not enough")
	}
	var length int
	firstByte := data[*offset]
	switch {
	case firstByte < 0xfb:
		length = int(firstByte)
		*offset++
	case firstByte == 0xfb:
		*offset++
		return "", nil
	case firstByte == 0xfc:
		if *offset+3 > len(data) {
			return "", errors.New("data length is not enough 1")
		}
		length = int(binary.LittleEndian.Uint16(data[*offset+1 : *offset+3]))
		*offset += 3
	case firstByte == 0xfd:
		if *offset+4 > len(data) {
			return "", errors.New("data length is not enough 2")
		}
		length = int(data[*offset+1]) | int(data[*offset+2])<<8 | int(data[*offset+3])<<16
		*offset += 4
	case firstByte == 0xfe:
		if *offset+9 > len(data) {
			return "", errors.New("data length is not enough 3")
		}
		length = int(binary.LittleEndian.Uint64(data[*offset+1 : *offset+9]))
		*offset += 9
	}
	result := string(data[*offset : *offset+length])
	*offset += length
	return result, nil
}
