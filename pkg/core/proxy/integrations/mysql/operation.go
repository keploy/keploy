//go:build linux

package mysql

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
)

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
			return nil, fmt.Errorf("invalid packet type for MySQLHandshakeV10: expected *MySQLHandshakeV10Packet, got %T", packet)
		}
		data, err = encodeHandshakePacket(p)
	case "HANDSHAKE_RESPONSE_OK":
		bypassHeader = true
		p, ok := packet.(*models.MySQLHandshakeResponseOk)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for HANDSHAKE_RESPONSE_OK: expected *MySQLHandshakeResponseOk, got %T", packet)
		}
		data, err = encodeHandshakeResponseOk(p)
	case "AUTH_SWITCH_REQUEST":
		p, ok := packet.(*models.AuthSwitchRequestPacket)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for AUTH_SWITCH_REQUEST: expected *AuthSwitchRequestPacket, got %T", packet)
		}
		data, err = encodeAuthSwitchRequest(p)
	case "AUTH_SWITCH_RESPONSE":
		p, ok := packet.(*models.AuthSwitchResponsePacket)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for AUTH_SWITCH_RESPONSE: expected *AuthSwitchResponsePacket, got %T", packet)
		}
		data, err = encodeAuthSwitchResponse(p)

	case "MySQLOK":
		p, ok := packet.(*models.MySQLOKPacket)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for MySQLOK: expected *MySQLOK, got %T", packet)
		}
		data, err = encodeMySQLOK(p, header)
		bypassHeader = true
	case "COM_STMT_PREPARE_OK":
		p, ok := packet.(*models.MySQLStmtPrepareOk)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for COM_STMT_PREPARE_OK: expected *MySQLStmtPrepareOk, got %T", packet)
		}
		data, err = encodeStmtPrepareOk(p)
		bypassHeader = true
	case "RESULT_SET_PACKET":
		p, ok := packet.(*models.MySQLResultSet)
		if !ok {
			return nil, fmt.Errorf("invalid packet for RESULT_SET_PACKET: expected *MySQLResultSet, got %T", packet)
		}
		data, err = encodeMySQLResultSet(p)
		bypassHeader = true
	case "MySQLErr":
		println("ERROR PACKET DETECTED")
		_, ok := packet.(*models.MySQLERRPacket)
		if !ok {
			return nil, fmt.Errorf("invalid packet type for MySQLErr: expected *MySQLErrPacket, got %T", packet)
		}
		println("Need to write encoding logic for err packet")
	default:
		return nil, errors.New("unknown operation type:" + operation)
	}

	if err != nil {
		return nil, err
	}
	if !bypassHeader {
		header := make([]byte, 4)
		binary.LittleEndian.PutUint32(header, uint32(len(data)))
		header[3] = byte(sequence)
		return append(header, data...), nil
	}

	return data, nil
}

func DecodeMySQLPacket(logger *zap.Logger, packet models.Packet, clientConn net.Conn, mode models.Mode, lastCommand *lastCommandMap, preparedStatements map[uint32]*models.MySQLStmtPrepareOk, serverGreetings *serverGreetings) (string, models.SQLPacketHeaderInfo, interface{}, error) {
	data := packet.Payload
	header := packet.Header
	var packetData interface{}
	var packetType string
	var err error

	if len(data) < 1 {
		return "", models.SQLPacketHeaderInfo{}, nil, fmt.Errorf("Invalid packet: Payload is empty")
	}

	lastCmd, ok := lastCommand.Load(clientConn)
	if !ok {
		lastCmd = 0x00
	}

	logger.Info("Last Cmd", zap.Any("LastCmd", lastCmd))
	fmt.Printf("Data before decoding: %v\n", data)
	switch {
	case (lastCmd == 0x03 || lastCmd == 0x17) && mode == models.MODE_RECORD:
		switch {
		case data[0] == 0x00: // OK Packet
			packetType = "MySQLOK"
			sg, ok := serverGreetings.load(clientConn)
			if !ok {
				return "", models.SQLPacketHeaderInfo{}, nil, fmt.Errorf("Server Greetings not found")
			}

			packetData, err = decodeMySQLOK(data, sg)
			lastCommand.Store(clientConn, 0x00) // Reset the last command

		case data[0] == 0xFF: // Error Packet
			packetType = "MySQLErr"
			sg, ok := serverGreetings.load(clientConn)
			if !ok {
				return "", models.SQLPacketHeaderInfo{}, nil, fmt.Errorf("Server Greetings not found")
			}
			packetData, err = decodeMySQLErr(data, sg)
			lastCommand.Store(clientConn, 0x00) // Reset the last command

		case data[0] == 0xFB: //LOCAL INFILE Data
			// packetType = "LOCAL_INFILE_DATA"
			// packetData, err = decodeLocalInfileData(data)
			lastCommand.Store(clientConn, 0x00) // Reset the last command
			return "", models.SQLPacketHeaderInfo{}, nil, fmt.Errorf("LOCAL INFILE DATA packet not supported")

		case isLengthEncodedInteger(data[0]): // ResultSet Packet
			logger.Info("ResultSet Packet detected")
			packetType = "RESULT_SET_PACKET"

			isBinary := false
			if lastCmd == 0x17 {
				isBinary = true
			}
			// text result set is used for COM_QUERY response
			// binary result set is used for COM_STMT_EXECUTE response

			packetData, err = parseResultSet(data, isBinary)
			if err != nil {
				logger.Error("Error parsing result set", zap.Error(err))
			}
			lastCommand.Store(clientConn, 0x00) // Reset the last command
		default:
			packetType = "Unknown"
			packetData = data
			logger.Debug("unknown packet type after COM_QUERY", zap.Int("unknownPacketTypeInt", int(data[0])))
		}
	case data[0] == 0x0e: // COM_PING
		packetType = "COM_PING"
		packetData, err = decodeComPing(data)
		lastCommand.Store(clientConn, 0x0e)
	case data[0] == 0x17: // COM_STMT_EXECUTE
		packetType = "COM_STMT_EXECUTE"
		packetData, err = decodeComStmtExecute(data, preparedStatements)
		lastCommand.Store(clientConn, 0x17)
	// case data[0] == 0x1c: // COM_STMT_FETCH
	// 	packetType = "COM_STMT_FETCH"
	// 	packetData, err = decodeComStmtFetch(data)
	// 	lastCommand.Store(clientConn, 0x1c)
	case data[0] == 0x16: // COM_STMT_PREPARE
		packetType = "COM_STMT_PREPARE"
		packetData, err = decodeComStmtPrepare(data)
		lastCommand.Store(clientConn, 0x16)
	case data[0] == 0x19: // COM_STMT_CLOSE
		if len(data) > 9 {
			if data[9] == 0x16 {
				println("COM_STMT_CLOSE_WITH_PREPARE packet detected")
				packetType = "COM_STMT_CLOSE_WITH_PREPARE"
				packetData, err = decodeComStmtCloseAndPrepare(data)
				lastCommand.Store(clientConn, 0x16)
			} else if data[9] == 0x03 {
				println("COM_STMT_CLOSE_WITH_QUERY packet detected")
				packetType = "COM_STMT_CLOSE_WITH_QUERY"
				packetData, err = decodeComStmtCloseAndQuery(data)
				lastCommand.Store(clientConn, 0x03)
			}
		} else {
			println("COM_STMT_CLOSE packet detected")
			packetType = "COM_STMT_CLOSE"
			packetData, err = decodeComStmtClose(data)
			lastCommand.Store(clientConn, 0x19)
		}
	case data[0] == 0x11: // COM_CHANGE_USER
		packetType = "COM_CHANGE_USER"
		packetData, err = decodeComChangeUser(data)
		lastCommand.Store(clientConn, 0x11)
	case data[0] == 0x0A: // MySQLHandshakeV10
		packetType = "MySQLHandshakeV10"
		packetData, err = decodeMySQLHandshakeV10(data)
		handshakePacket, _ := packetData.(*models.MySQLHandshakeV10Packet)
		serverGreetings.store(clientConn, handshakePacket)
		handshakePluginName = handshakePacket.AuthPluginName
		logger.Info("Detected MYSQLHanshakeV10 packet", zap.String("AuthPluginName", handshakePluginName))
		lastCommand.Store(clientConn, 0x0A)
	case data[0] == 0x03: // MySQLQuery
		packetType = "MySQLQuery"
		packetData, err = decodeMySQLQuery(data)
		lastCommand.Store(clientConn, 0x03)
	case data[0] == 0x00: // MySQLOK or COM_STMT_PREPARE_OK
		lastCmd, ok := lastCommand.Load(clientConn)
		if ok && lastCmd == 0x16 {
			packetType = "COM_STMT_PREPARE_OK"
			packetData, err = decodeComStmtPrepareOk(data)
			if err == nil {
				prepareOk := packetData.(*models.MySQLStmtPrepareOk)
				preparedStatements[prepareOk.StatementID] = prepareOk
			}
		} else {
			packetType = "MySQLOK"
			sg, ok := serverGreetings.load(clientConn)
			if !ok {
				return "", models.SQLPacketHeaderInfo{}, nil, fmt.Errorf("Server Greetings not found")
			}
			packetData, err = decodeMySQLOK(data, sg)
			logger.Info("MySQLOK packet detected")
		}
		lastCommand.Store(clientConn, 0x00)
	case data[0] == 0xFF: // MySQLErr
		packetType = "MySQLErr"
		logger.Info("MySQLErr packet detected")
		sg, ok := serverGreetings.load(clientConn)
		if !ok {
			return "", models.SQLPacketHeaderInfo{}, nil, fmt.Errorf("Server Greetings not found")
		}
		packetData, err = decodeMySQLErr(data, sg)
		lastCommand.Store(clientConn, 0xFF)
	case data[0] == 0xFE && len(data) > 5: // Auth Switch Packet
		packetType = "AUTH_SWITCH_REQUEST"
		packetData, err = decodeAuthSwitchRequest(data)
		lastCommand.Store(clientConn, 0xFE)
		logger.Info("Auth Switch Request Packet detected")
	case data[0] == 0xFE || expectingAuthSwitchResponse:
		packetType = "AUTH_SWITCH_RESPONSE"
		packetData, err = decodeAuthSwitchResponse(data)
		logger.Info("Auth Switch Response Packet detected")
		expectingAuthSwitchResponse = false
	case data[0] == 0xFE: // EOF packet
		packetType = "MySQLEOF"
		sg, ok := serverGreetings.load(clientConn)
		if !ok {
			return "", models.SQLPacketHeaderInfo{}, nil, fmt.Errorf("Server Greetings not found")
		}
		packetData, err = decodeMYSQLEOF(data, sg)
		lastCommand.Store(clientConn, 0xFE)
		logger.Info("EOF packet detected", zap.Error(err))
	case data[0] == 0x02: // New packet type
		if len(data) == 1 {
			packetType = "REQUEST_PUBLIC_KEY"
			packetData = nil
			lastCommand.Store(clientConn, 0x02)
			logger.Info("REQUEST_PUBLIC_KEY packet detected")
		} else {
			// packetType = "AuthNextFactor"
			packetType = "AUTH_NEXT_FACTOR"
			err = decodeAuthNextFactor(data)
			lastCommand.Store(clientConn, 0x02)
			logger.Info("AUTH_NEXT_FACTOR packet detected")
		}
	case data[0] == 0x18: // SEND_LONG_DATA Packet
		packetType = "COM_STMT_SEND_LONG_DATA"
		packetData, err = decodeComStmtSendLongData(data)
		lastCommand.Store(clientConn, 0x18)
	case data[0] == 0x1a: // STMT_RESET Packet
		packetType = "COM_STMT_RESET"
		packetData, err = decodeComStmtReset(data)
		lastCommand.Store(clientConn, 0x1a)
	case data[0] == 0x8d || expectingHandshakeResponse || expectingHandshakeResponseTest: // Handshake Response packet
		packetType = "HANDSHAKE_RESPONSE"
		packetData, err = decodeHandshakeResponse(data)
		lastCommand.Store(clientConn, 0x8d) // This value may differ depending on the handshake response protocol version
		logger.Info("client Handshake Response packet detected")
	case data[0] == 0x01: // Handshake Response packet
		if len(data) == 1 {
			packetType = "COM_QUIT"
			packetData = nil
		} else {
			packetType = "HANDSHAKE_RESPONSE_OK"
			packetData, err = decodeHandshakeResponseOk(data)
		}
	default:
		packetType = "Unknown"
		packetData = data
		logger.Debug("unknown packet type", zap.Int("unknownPacketTypeInt", int(data[0])))
	}

	if err != nil {
		return "", models.SQLPacketHeaderInfo{}, nil, err
	}
	if models.GetMode() != "test" {
		logger.Debug("Packet Info",
			zap.String("PacketType", packetType),
			zap.ByteString("Data", data))
	}
	if (models.GetMode()) == "test" {
		lastCommand.Store(clientConn, 0x00)
	}
	return packetType, header, packetData, nil
}

func isLengthEncodedInteger(b byte) bool {
	// This is a simplified check. You may need a more robust check based on MySQL protocol.
	return b != 0x00 && b != 0xFF && b != 0xFB
}

func Encode(p *models.Packet) ([]byte, error) {
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
		queryJSON, _ := json.Marshal(queryObj)
		packet = append(packet, queryJSON...)
	}

	return packet, nil
}

type serverGreetings struct {
	sync.RWMutex
	internal map[net.Conn]*models.MySQLHandshakeV10Packet
}

func newServerGreetings() *serverGreetings {
	return &serverGreetings{
		internal: make(map[net.Conn]*models.MySQLHandshakeV10Packet),
	}
}

func (sg *serverGreetings) load(key net.Conn) (*models.MySQLHandshakeV10Packet, bool) {
	sg.RLock()
	result, ok := sg.internal[key]
	sg.RUnlock()
	return result, ok
}

func (sg *serverGreetings) store(key net.Conn, value *models.MySQLHandshakeV10Packet) {
	sg.Lock()
	sg.internal[key] = value
	sg.Unlock()
}

type lastCommandMap struct {
	sync.RWMutex
	internal map[net.Conn]byte
}

func newlastCommandMap() *lastCommandMap {
	return &lastCommandMap{
		internal: make(map[net.Conn]byte),
	}
}

func (rm *lastCommandMap) Load(key net.Conn) (value byte, ok bool) {
	rm.RLock()
	result, ok := rm.internal[key]
	rm.RUnlock()
	return result, ok
}

func (rm *lastCommandMap) Delete(key net.Conn) {
	rm.Lock()
	delete(rm.internal, key)
	rm.Unlock()
}

func (rm *lastCommandMap) Store(key net.Conn, value byte) {
	rm.Lock()
	rm.internal[key] = value
	rm.Unlock()
}

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
		err := binary.Write(buf, binary.LittleEndian, uint16(length))
		if err != nil {
			return
		}
	case length <= 0xFFFFFF:
		buf.WriteByte(0xFD)
		err := binary.Write(buf, binary.LittleEndian, uint32(length)&0xFFFFFF)
		if err != nil {
			return
		}
	default:
		buf.WriteByte(0xFE)
		err := binary.Write(buf, binary.LittleEndian, uint64(length))
		if err != nil {
			return
		}
	}
	buf.WriteString(s)
}

func writeLengthEncodedInteger(buf *bytes.Buffer, val *uint64) {
	if val == nil {
		// Write 0xFB to represent NULL
		buf.WriteByte(0xFB)
		return
	}

	switch {
	case *val <= 250:
		buf.WriteByte(byte(*val))
	case *val <= 0xFFFF:
		buf.WriteByte(0xFC)
		err := binary.Write(buf, binary.LittleEndian, uint16(*val))
		if err != nil {
			return
		}
	case *val <= 0xFFFFFF:
		buf.WriteByte(0xFD)
		err := binary.Write(buf, binary.LittleEndian, uint32(*val)&0xFFFFFF)
		if err != nil {
			return
		}
	default:
		buf.WriteByte(0xFE)
		err := binary.Write(buf, binary.LittleEndian, *val)
		if err != nil {
			return
		}
	}
}

//func writeLengthEncodedIntegers(buf *bytes.Buffer, value *uint64) {
//	if value == nil {
//		// Write 0xFB to represent NULL
//		buf.WriteByte(0xFB)
//		return
//	}
//
//	if *value <= 250 {
//		buf.WriteByte(byte(*value))
//	} else if *value <= 0xffff {
//		buf.WriteByte(0xfc)
//		buf.WriteByte(byte(*value))
//		buf.WriteByte(byte(*value >> 8))
//	} else if *value <= 0xffffff {
//		buf.WriteByte(0xfd)
//		buf.WriteByte(byte(*value))
//		buf.WriteByte(byte(*value >> 8))
//		buf.WriteByte(byte(*value >> 16))
//	} else {
//		buf.WriteByte(0xfe)
//		binary.Write(buf, binary.LittleEndian, *value)
//	}
//}

//func writeLengthEncodedStrings(buf *bytes.Buffer, value string) {
//	data := []byte(value)
//	length := uint64(len(data))
//	writeLengthEncodedIntegers(buf, &length)
//	buf.Write(data)
//}

//func nullTerminatedString(data []byte) (string, int, error) {
//	pos := bytes.IndexByte(data, 0)
//	if pos == -1 {
//		return "", 0, errors.New("null-terminated string not found")
//	}
//	return string(data[:pos]), pos, nil
//}

func readLengthEncodedInteger(b []byte) (num uint64, isNull bool, n int) {
	if len(b) == 0 {
		return 0, true, 0
	}

	switch b[0] {
	// 251: NULL
	case 0xfb:
		return 0, true, 1

		// 252: value of following 2
	case 0xfc:
		return uint64(b[1]) | uint64(b[2])<<8, false, 3

		// 253: value of following 3
	case 0xfd:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16, false, 4

		// 254: value of following 8
	case 0xfe:
		return uint64(b[1]) | uint64(b[2])<<8 | uint64(b[3])<<16 |
				uint64(b[4])<<24 | uint64(b[5])<<32 | uint64(b[6])<<40 |
				uint64(b[7])<<48 | uint64(b[8])<<56,
			false, 9
	}

	// 0-250: value of first byte
	return uint64(b[0]), false, 1
}

func isEOFPacket(data []byte) bool {
	return len(data) > 4 && bytes.Contains(data[4:9], []byte{0xfe, 0x00, 0x00})
}

func readUint24(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}

func readLengthEncodedString(b []byte) ([]byte, bool, int, error) {
	// Get length
	num, isNull, n := readLengthEncodedInteger(b)
	if num < 1 {
		return b[n:n], isNull, n, nil
	}

	n += int(num)

	// Check data length
	if len(b) >= n {
		return b[n-int(num) : n : n], false, n, nil
	}
	return nil, false, n, io.EOF
}

func ShouldUseSSL(packet *models.MySQLHandshakeV10Packet) bool {
	return (packet.CapabilityFlags & models.CLIENT_SSL) != 0
}

func GetAuthMethod(packet *models.MySQLHandshakeV10Packet) string {
	// It will return the auth method
	return packet.AuthPluginName
}

func Uint24(data []byte) uint32 {
	return uint32(data[0]) | uint32(data[1])<<8 | uint32(data[2])<<16
}
