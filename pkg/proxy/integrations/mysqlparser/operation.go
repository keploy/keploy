package mysqlparser

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"go.keploy.io/server/pkg/models"
	"go.uber.org/zap"
)

type MySQLPacketHeader struct {
	PayloadLength uint32 `yaml:"payload_length"` // MySQL packet payload length
	SequenceID    uint8  `yaml:"sequence_id"`    // MySQL packet sequence ID
}

type MySQLPacket struct {
	Header  MySQLPacketHeader `yaml:"header"`
	Payload []byte            `yaml:"payload"`
}

type HandshakeResponse41 struct {
	CapabilityFlags   CapabilityFlags   `yaml:"capability_flags"`
	MaxPacketSize     uint32            `yaml:"max_packet_size"`
	CharacterSet      uint8             `yaml:"character_set"`
	Reserved          [23]byte          `yaml:"reserved"`
	Username          string            `yaml:"username"`
	LengthEncodedInt  uint8             `yaml:"length_encoded_int"`
	AuthResponse      []byte            `yaml:"auth_response"`
	Database          string            `yaml:"database"`
	AuthPluginName    string            `yaml:"auth_plugin_name"`
	ConnectAttributes map[string]string `yaml:"connect_attributes"`
}

type SSLRequestPacket struct {
	Capabilities  uint32   `yaml:"capabilities"`
	MaxPacketSize uint32   `yaml:"max_packet_size"`
	CharacterSet  uint8    `yaml:"character_set"`
	Reserved      [23]byte `yaml:"reserved"`
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

type ColumnValue struct {
	Null  bool   `yaml:"null"`
	Value string `yaml:"value"`
}

type ColumnDefinitionPacket struct {
	Catalog      string `yaml:"catalog"`
	Schema       string `yaml:"schema"`
	Table        string `yaml:"table"`
	OrgTable     string `yaml:"org_table"`
	Name         string `yaml:"name"`
	OrgName      string `yaml:"org_name"`
	CharacterSet uint16 `yaml:"character_set"`
	ColumnLength uint32 `yaml:"column_length"`
	ColumnType   string `yaml:"column_type"`
	Flags        uint16 `yaml:"flags"`
	Decimals     uint8  `yaml:"decimals"`
	Filler       uint16 `yaml:"filler"`
	DefaultValue string `yaml:"default_value"`
}

type packetDecoder struct {
	conn net.Conn `yaml:"conn"`
}

type binaryRows struct {
	pd      *packetDecoder `yaml:"pd"`
	rs      resultSet      `yaml:"rs"`
	mc      mysqlConn      `yaml:"mc"`
	data    []byte         `yaml:"data"`
	columns []mysqlField   `yaml:"columns"`
}

type resultSet struct {
	columns []column `yaml:"columns"`
	done    bool     `yaml:"done"`
}

type column struct {
	fieldType int `yaml:"field_type"`
	flags     int `yaml:"flags"`
	decimals  int `yaml:"decimals"`
}

type mysqlConn struct {
	status uint16 `yaml:"status"`
	cfg    config `yaml:"cfg"`
}

type config struct {
	Loc int `yaml:"loc"`
}

type mysqlField struct {
	tableName string    `yaml:"table_name"`
	name      string    `yaml:"name"`
	length    uint32    `yaml:"length"`
	flags     fieldFlag `yaml:"flags"`
	fieldType fieldType `yaml:"field_type"`
	decimals  byte      `yaml:"decimals"`
	charSet   uint8     `yaml:"char_set"`
}

type ResultsetRowPacket struct {
	ColumnValues []string `yaml:"column_values"`
	RowValues    []string `yaml:"row_values"`
}

type COM_STMT_RESET struct {
	StatementID uint32 `yaml:"statement_id"`
}

type PluginDetails struct {
	Type    string `yaml:"type"`
	Message string `yaml:"message"`
}

type CapabilityFlags uint32

var mySQLfieldTypeNames = map[byte]string{
	0x00: "MYSQL_TYPE_DECIMAL",
	0x01: "MYSQL_TYPE_TINY",
	0x02: "MYSQL_TYPE_SHORT",
	0x03: "MYSQL_TYPE_LONG",
	0x04: "MYSQL_TYPE_FLOAT",
	0x05: "MYSQL_TYPE_DOUBLE",
	0x06: "MYSQL_TYPE_NULL",
	0x07: "MYSQL_TYPE_TIMESTAMP",
	0x08: "MYSQL_TYPE_LONGLONG",
	0x09: "MYSQL_TYPE_INT24",
	0x0a: "MYSQL_TYPE_DATE",
	0x0b: "MYSQL_TYPE_TIME",
	0x0c: "MYSQL_TYPE_DATETIME",
	0x0d: "MYSQL_TYPE_YEAR",
	0x0e: "MYSQL_TYPE_NEWDATE",
	0x0f: "MYSQL_TYPE_VARCHAR",
	0x10: "MYSQL_TYPE_BIT",
	0xf6: "MYSQL_TYPE_NEWDECIMAL",
	0xf7: "MYSQL_TYPE_ENUM",
	0xf8: "MYSQL_TYPE_SET",
	0xf9: "MYSQL_TYPE_TINY_BLOB",
	0xfa: "MYSQL_TYPE_MEDIUM_BLOB",
	0xfb: "MYSQL_TYPE_LONG_BLOB",
	0xfc: "MYSQL_TYPE_BLOB",
	0xfd: "MYSQL_TYPE_VAR_STRING",
	0xfe: "MYSQL_TYPE_STRING",
	0xff: "MYSQL_TYPE_GEOMETRY",
}
var columnTypeValues = map[string]byte{
	"MYSQL_TYPE_DECIMAL":     0x00,
	"MYSQL_TYPE_TINY":        0x01,
	"MYSQL_TYPE_SHORT":       0x02,
	"MYSQL_TYPE_LONG":        0x03,
	"MYSQL_TYPE_FLOAT":       0x04,
	"MYSQL_TYPE_DOUBLE":      0x05,
	"MYSQL_TYPE_NULL":        0x06,
	"MYSQL_TYPE_TIMESTAMP":   0x07,
	"MYSQL_TYPE_LONGLONG":    0x08,
	"MYSQL_TYPE_INT24":       0x09,
	"MYSQL_TYPE_DATE":        0x0a,
	"MYSQL_TYPE_TIME":        0x0b,
	"MYSQL_TYPE_DATETIME":    0x0c,
	"MYSQL_TYPE_YEAR":        0x0d,
	"MYSQL_TYPE_NEWDATE":     0x0e,
	"MYSQL_TYPE_VARCHAR":     0x0f,
	"MYSQL_TYPE_BIT":         0x10,
	"MYSQL_TYPE_NEWDECIMAL":  0xf6,
	"MYSQL_TYPE_ENUM":        0xf7,
	"MYSQL_TYPE_SET":         0xf8,
	"MYSQL_TYPE_TINY_BLOB":   0xf9,
	"MYSQL_TYPE_MEDIUM_BLOB": 0xfa,
	"MYSQL_TYPE_LONG_BLOB":   0xfb,
	"MYSQL_TYPE_BLOB":        0xfc,
	"MYSQL_TYPE_VAR_STRING":  0xfd,
	"MYSQL_TYPE_STRING":      0xfe,
	"MYSQL_TYPE_GEOMETRY":    0xff,
}

var handshakePluginName string

func NewHandshakeResponsePacket(handshake *HandshakeV10Packet, authMethod string, password string) *HandshakeResponse41 {
	authResponse := GenerateAuthResponse(password, handshake.AuthPluginData)
	return &HandshakeResponse41{
		CapabilityFlags: CapabilityFlags(handshake.CapabilityFlags),
		MaxPacketSize:   MaxPacketSize,
		CharacterSet:    0x21, // utf8_general_ci
		Username:        "user",
		AuthResponse:    authResponse,
		Database:        "shorturl_db",
		AuthPluginName:  authMethod,
	}
}
func GenerateAuthResponse(password string, salt []byte) []byte {
	// 1. Hash the password
	passwordHash := sha1.Sum([]byte(password))

	// 2. Hash the salt and the password hash
	finalHash := sha1.Sum(append(salt, passwordHash[:]...))

	return finalHash[:]
}

func (p *HandshakeResponse41) EncodeHandshake() ([]byte, error) {
	length := 4 + 4 + 1 + 23 + len(p.Username) + 1 + 1 + len(p.AuthResponse) + len(p.Database) + 1 + len(p.AuthPluginName) + 1
	buffer := make([]byte, length)
	offset := 0

	binary.LittleEndian.PutUint32(buffer[offset:], uint32(p.CapabilityFlags))
	offset += 4
	binary.LittleEndian.PutUint32(buffer[offset:], p.MaxPacketSize)
	offset += 4
	buffer[offset] = p.CharacterSet
	offset += 1 + 23
	offset += copy(buffer[offset:], p.Username)
	buffer[offset] = 0x00
	offset++
	buffer[offset] = uint8(len(p.AuthResponse))
	offset++
	offset += copy(buffer[offset:], p.AuthResponse)
	offset += copy(buffer[offset:], p.Database)
	buffer[offset] = 0x00
	offset++
	offset += copy(buffer[offset:], p.AuthPluginName)
	buffer[offset] = 0x00

	return buffer, nil
}

func NewSSLRequestPacket(capabilities uint32, maxPacketSize uint32, characterSet uint8) *SSLRequestPacket {
	// Ensure the SSL capability flag is set
	capabilities |= CLIENT_SSL

	if characterSet == 0 {
		characterSet = 33 // Set default to utf8mb4 if not specified.
	}

	return &SSLRequestPacket{
		Capabilities:  capabilities,
		MaxPacketSize: maxPacketSize,
		CharacterSet:  characterSet,
		Reserved:      [23]byte{},
	}
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

func encodeColumnDefinition(buf *bytes.Buffer, column *models.ColumnDefinition, seqNum *byte) error {
	tmpBuf := &bytes.Buffer{}
	writeLengthEncodedString(tmpBuf, column.Catalog)
	writeLengthEncodedString(tmpBuf, column.Schema)
	writeLengthEncodedString(tmpBuf, column.Table)
	writeLengthEncodedString(tmpBuf, column.OrgTable)
	writeLengthEncodedString(tmpBuf, column.Name)
	writeLengthEncodedString(tmpBuf, column.OrgName)
	tmpBuf.WriteByte(0x0C)
	if err := binary.Write(tmpBuf, binary.LittleEndian, column.CharacterSet); err != nil {
		return err
	}
	if err := binary.Write(tmpBuf, binary.LittleEndian, column.ColumnLength); err != nil {
		return err
	}
	tmpBuf.WriteByte(column.ColumnType)
	if err := binary.Write(tmpBuf, binary.LittleEndian, column.Flags); err != nil {
		return err
	}
	tmpBuf.WriteByte(column.Decimals)
	tmpBuf.Write([]byte{0x00, 0x00})

	colData := tmpBuf.Bytes()
	length := len(colData)

	// Write packet header with length and sequence number
	buf.WriteByte(byte(length))
	buf.WriteByte(byte(length >> 8))
	buf.WriteByte(byte(length >> 16))
	buf.WriteByte(*seqNum)
	*seqNum++

	// Write column definition data
	buf.Write(colData)

	return nil
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

func encodeRow(row *models.Row, columnValues []models.RowColumnDefinition) ([]byte, error) {
	var buf bytes.Buffer

	// Write the header
	//binary.Write(&buf, binary.LittleEndian, uint32(row.Header.PacketLength))
	//buf.WriteByte(row.Header.PacketSequenceId)

	for _, column := range columnValues {
		value := column.Value
		switch fieldType(column.Type) {
		case fieldTypeTimestamp:
			timestamp, ok := value.(string)
			if !ok {
				return nil, errors.New("could not convert value to string")
			}
			t, err := time.Parse("2006-01-02 15:04:05", timestamp)
			if err != nil {
				return nil, errors.New("could not parse timestamp value")
			}

			buf.WriteByte(7) // Length of the following encoded data
			yearBytes := make([]byte, 2)
			binary.LittleEndian.PutUint16(yearBytes, uint16(t.Year()))
			buf.Write(yearBytes)            // Year
			buf.WriteByte(byte(t.Month()))  // Month
			buf.WriteByte(byte(t.Day()))    // Day
			buf.WriteByte(byte(t.Hour()))   // Hour
			buf.WriteByte(byte(t.Minute())) // Minute
			buf.WriteByte(byte(t.Second())) // Second
		default:
			strValue, ok := value.(string)
			if !ok {
				return nil, errors.New("could not convert value to string")
			}
			// Write a length-encoded integer for the string length
			writeLengthEncodedInteger(&buf, uint64(len(strValue)))
			// Write the string
			buf.WriteString(strValue)
		}
	}

	return buf.Bytes(), nil
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

func encodeToBinary(packet interface{}, operation string, sequence int) ([]byte, error) {
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
	case "MySQLOK":
		p, ok := packet.(*models.MySQLOKPacket)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for HandshakeResponse: expected *HandshakeResponse, got %T", packet)
		}
		data, err = encodeMySQLOK(p)
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
		fmt.Println(uint32(len(data)))
		return append(header, data...), nil
	} else {
		return data, nil
	}
}

func DecodeMySQLPacket(packet MySQLPacket, logger *zap.Logger, destConn net.Conn) (string, MySQLPacketHeader, interface{}, error) {

	data := packet.Payload
	header := packet.Header

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
		packetType = "COM_STMT_CLOSE"
		packetData, err = decodeComStmtClose(data)
		lastCommand = 0x19
	case data[0] == 0x11: // COM_CHANGE_USER
		packetType = "COM_CHANGE_USER"
		packetData, err = decodeComChangeUser(data)
		lastCommand = 0x11
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
		packetType = "AuthSwitchRequest"
		packetData, err = decodeAuthSwitchRequest(data)
		lastCommand = 0xFE
	case data[0] == 0xFE: // EOF packet
		packetType = "MySQLEOF"
		packetData, err = decodeMYSQLEOF(data)
		lastCommand = 0xFE
	case data[0] == 0x02: // New packet type
		packetType = "NewPacketType2"
		packetData, err = decodePacketType2(data)
		lastCommand = 0x02
	case data[0] == 0x18: // SEND_LONG_DATA Packet
		packetType = "COM_STMT_SEND_LONG_DATA"
		packetData, err = decodeComStmtSendLongData(data)
		lastCommand = 0x18
	case data[0] == 0x1a: // STMT_RESET Packet
		packetType = "COM_STMT_RESET"
		packetData, err = decodeComStmtReset(data)
		lastCommand = 0x1a
	case data[0] == 0x8d: // Handshake Response packet
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

	return packetType, header, packetData, nil
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

type PacketHeader struct {
	PacketLength     uint8 `yaml:"packet_length"`
	PacketSequenceID uint8 `yaml:"packet_sequence_id"`
}
type RowHeader struct {
	PacketLength int   `yaml:"packet_length"`
	SequenceID   uint8 `yaml:"sequence_id"`
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

func decodeRowData(data []byte, columns []ColumnDefinition) ([]RowDataPacket, []byte, error) {
	var rowPackets []RowDataPacket
	for _, _ = range columns {
		var rowData RowDataPacket
		var err error

		// Check for NULL column
		if data[0] == 0xfb {
			data = data[1:]
			rowData.Data = nil
			rowPackets = append(rowPackets, rowData)
			continue
		}

		var fieldStr string
		fieldStr, data, err = readLengthEncodedStringUpdated(data)
		if err != nil {
			return nil, nil, err
		}

		rowData.Data = []byte(fieldStr)
		rowPackets = append(rowPackets, rowData)
	}

	return rowPackets, data, nil
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

type fieldFlag uint16

func (packet *HandshakeV10Packet) ShouldUseSSL() bool {
	return (packet.CapabilityFlags & CLIENT_SSL) != 0
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
