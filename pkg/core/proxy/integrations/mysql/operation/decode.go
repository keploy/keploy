//go:build linux

// Package operation provides encoding and decoding of MySQL packets.
package operation

import (
	"context"
	"fmt"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command/preparedstmt"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command/utility"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/connection"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/generic"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

/*
    1.  BytesToMySQLStruct
	2.	DecodeMySQLBytes
	3.	ParseMySQLPacket
	4.	MySQLBytesToStruct
	5.	UnmarshalMySQLPacket
	6.	ConvertBytesToMySQL
	7.	DeserializeMySQLPacket
	8.	DecodeMySQLData
	9.	BytesToMySQLData
	10.	UnpackMySQLBytes
*/

type DecodeContext struct {
	Mode               models.Mode
	LastOp             *LastOperation
	PreparedStatements map[uint32]*mysql.StmtPrepareOkPacket
	ServerGreetings    *ServerGreetings
}

func DecodePayload(ctx context.Context, logger *zap.Logger, data []byte, clientConn net.Conn, decodeCtx *DecodeContext) (*mysql.PacketBundle, error) {
	//Parse the data into mysql header and payload
	packet, err := utils.BytesToMySQLPacket(data)
	if err != nil {
		return &mysql.PacketBundle{}, fmt.Errorf("failed to parse MySQL packet: %w", err)
	}

	if len(packet.Payload) < 1 {
		return &mysql.PacketBundle{}, fmt.Errorf("invalid packet, payload is empty")
	}

	lastOp, ok := decodeCtx.LastOp.Load(clientConn)
	if !ok {
		println("lastOp not found in decodepayload")
		lastOp = 0x00
	}

	//debug log
	logger.Info("Last operation in DecodePayload", zap.String("operation", fmt.Sprintf("%#x", lastOp)), zap.Any("header", packet.Header))

	println("Mode", decodeCtx.Mode)

	if (lastOp == mysql.COM_QUERY || lastOp == mysql.COM_STMT_EXECUTE) && decodeCtx.Mode == models.MODE_RECORD {
		return handleQueryStmtResponse(ctx, logger, packet, clientConn, lastOp, decodeCtx)
	}

	parsedPacket, err := decodePacket(ctx, logger, packet, clientConn, lastOp, decodeCtx)
	if err != nil {
		return &mysql.PacketBundle{}, fmt.Errorf("failed to decode packet: %w", err)
	}

	return parsedPacket, nil
}

func handleQueryStmtResponse(ctx context.Context, logger *zap.Logger, packet mysql.Packet, clientConn net.Conn, lastOp byte, decodeCtx *DecodeContext) (*mysql.PacketBundle, error) {
	//Get the Header & payload of the packet
	header := packet.Header
	payload := packet.Payload

	parsedPacket := &mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &header,
		},
	}

	payloadType := payload[0]

	sg, ok := decodeCtx.ServerGreetings.Load(clientConn)
	if !ok {
		return parsedPacket, fmt.Errorf("Server Greetings not found")
	}

	//debug log
	logger.Info("Last operation when handling client query", zap.Any("last operation", mysql.CommandStatusToString(lastOp)))

	switch payloadType {
	case mysql.OK:
		pkt, err := generic.DecodeOk(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode OK packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.OK), clientConn, RESET, decodeCtx)

	case mysql.ERR:

		pkt, err := generic.DecodeErr(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode ERR packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.ERR), clientConn, RESET, decodeCtx)

	case mysql.LocalInFile:
		parsedPacket.Header.Type = "LocalInFile"
		decodeCtx.LastOp.Store(clientConn, RESET) //reset the last operation
		return parsedPacket, fmt.Errorf("LocalInFile not supported")
	default:
		//If the packet is not OK, ERR or LocalInFile, then it is a result set
		var pktType string
		var rowType command.RowType
		if lastOp == mysql.COM_STMT_EXECUTE {
			rowType = command.Binary
			pktType = string(mysql.Binary)
		} else {
			rowType = command.Text
			pktType = string(mysql.Text)
		}

		pkt, err := command.DecodeResultSet(ctx, logger, payload, rowType)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode result set: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, pktType, clientConn, RESET, decodeCtx)
	}

	return parsedPacket, nil
}

func decodePacket(ctx context.Context, logger *zap.Logger, packet mysql.Packet, clientConn net.Conn, lastOp byte, decodeCtx *DecodeContext) (*mysql.PacketBundle, error) {
	//Get the Header & payload of the packet
	header := packet.Header
	payload := packet.Payload

	parsedPacket := &mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &header,
		},
	}

	payloadType := payload[0]

	var sg *mysql.HandshakeV10Packet
	var ok bool
	// No need to find the server greetings in the map if the payload is HandshakeV10 because it is the first packet and going to be stored in the map
	if payloadType != mysql.HandshakeV10 {
		sg, ok = decodeCtx.ServerGreetings.Load(clientConn)
		if !ok {
			return parsedPacket, fmt.Errorf("Server Greetings not found")
		}
	}

	//debug log
	logger.Info("payload info", zap.Any("last operation", lastOp), zap.Any("payload type", payloadType))

	// Handle handshakeResponse41 separately, because its status is not defined and can be changed with the client capabilities.
	if lastOp == mysql.HandshakeV10 {
		//debug log
		logger.Info("HandshakeResponse41 packet", zap.Any("Type", payloadType))
		pkt, err := connection.DecodeHandshakeResponse41(ctx, logger, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode HandshakeResponse41 packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.HandshakeResponse41, clientConn, payloadType, decodeCtx)

		if decodeCtx.Mode == models.MODE_TEST {
			decodeCtx.LastOp.Store(clientConn, RESET) //reset the last operation
		}
		return parsedPacket, nil
	}

	switch {
	// generic response packets
	case payloadType == mysql.EOF:
		logger.Debug("EOF packet", zap.Any("Type", payloadType))
		pkt, err := generic.DecodeEOF(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode EOF packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.EOF), clientConn, mysql.EOF, decodeCtx)

	case payloadType == mysql.ERR:
		logger.Debug("ERR packet", zap.Any("Type", payloadType))
		pkt, err := generic.DecodeErr(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode ERR packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.ERR), clientConn, mysql.ERR, decodeCtx)

	case payloadType == mysql.OK:
		if lastOp == mysql.COM_STMT_PREPARE {
			logger.Debug("COM_STMT_PREPARE_OK packet", zap.Any("Type", payloadType))
			pkt, err := preparedstmt.DecodePrepareOk(ctx, logger, payload)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode COM_STMT_PREPARE_OK packet: %w", err)
			}

			setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.OK), clientConn, mysql.OK, decodeCtx)
			// Store the prepared statement to use it later
			decodeCtx.PreparedStatements[pkt.StatementID] = pkt
			//debug log
			logger.Info("Prepared statement stored", zap.Any("statementId", pkt.StatementID), zap.Any("prepared statement", pkt))

		} else {
			logger.Debug("OK packet", zap.Any("Type", payloadType))
			pkt, err := generic.DecodeOk(ctx, payload, sg.CapabilityFlags)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode OK packet: %w", err)
			}

			setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.OK), clientConn, mysql.OK, decodeCtx)
		}

		// auth packets
	case payloadType == 0x01:
		if len(payload) == 1 {
			logger.Debug("COM_QUIT packet", zap.Any("Type", payloadType))
			pkt := &mysql.QuitPacket{
				Command: payloadType,
			}
			setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_QUIT), clientConn, mysql.COM_QUIT, decodeCtx)
		} else {
			//otherwise it is a AuthMoreData packet
			logger.Debug("AuthMoreData packet", zap.Any("Type", payloadType))
			pkt, err := connection.DecodeAuthMoreData(ctx, payload)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode AuthMoreData packet: %w", err)
			}
			setPacketInfo(ctx, parsedPacket, pkt, mysql.AuthStatusToString(mysql.AuthMoreData), clientConn, mysql.AuthMoreData, decodeCtx)
		}
	case payloadType == mysql.AuthSwitchRequest && len(payload) > 5:
		logger.Debug("AuthSwitchRequest packet", zap.Any("Type", payloadType))
		return parsedPacket, fmt.Errorf("AuthSwitchRequest not supported")
	case payloadType == 0x02:
		if len(payload) == 1 {
			logger.Debug(("Request public key detected"))
			setPacketInfo(ctx, parsedPacket, "request_public_key", mysql.CachingSha2PasswordToString(mysql.RequestPublicKey), clientConn, byte(mysql.RequestPublicKey), decodeCtx)
		} else {
			logger.Debug("AuthNextFactor packet", zap.Any("Type", payloadType))
			pkt, err := connection.DecodeAuthNextFactor(ctx, payload)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode AuthNextFactor packet: %w", err)
			}
			setPacketInfo(ctx, parsedPacket, pkt, mysql.AuthStatusToString(mysql.AuthNextFactor), clientConn, mysql.AuthNextFactor, decodeCtx)
		}
	case payloadType == mysql.HandshakeV10:
		logger.Debug("HandshakeV10 packet", zap.Any("Type", payloadType))
		pkt, err := connection.DecodeHandshakeV10(ctx, logger, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode HandshakeV10 packet: %w", err)
		}
		// Store the server greetings to use it later
		decodeCtx.ServerGreetings.store(clientConn, pkt)
		setPacketInfo(ctx, parsedPacket, pkt, mysql.AuthStatusToString(mysql.HandshakeV10), clientConn, mysql.HandshakeV10, decodeCtx)
	// case payloadType == 0x8d:
	// 	//debug log
	// 	logger.Info("HandshakeResponse41 packet", zap.Any("Type", payloadType))
	// 	pkt, err := connection.DecodeHandshakeResponse41(ctx, logger, payload)
	// 	if err != nil {
	// 		return parsedPacket, fmt.Errorf("failed to decode HandshakeResponse41 packet: %w", err)
	// 	}

	// 	printByteArray("HandshakeResponse41", payload)

	// 	setPacketInfo(ctx, parsedPacket, pkt, mysql.HandshakeResponse41, clientConn, 0x8d, decodeCtx)

	// utility packets
	case payloadType == mysql.COM_QUIT:
		logger.Debug("COM_QUIT packet", zap.Any("Type", payloadType))
		pkt := &mysql.QuitPacket{
			Command: payloadType,
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_QUIT), clientConn, mysql.COM_QUIT, decodeCtx)

	case payloadType == mysql.COM_INIT_DB:
		logger.Debug("COM_INIT_DB packet", zap.Any("Type", payloadType))
		pkt, err := utility.DecodeInitDb(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_INIT_DB packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_INIT_DB), clientConn, mysql.COM_INIT_DB, decodeCtx)

	case payloadType == mysql.COM_STATISTICS:
		logger.Debug("COM_STATISTICS packet", zap.Any("Type", payloadType))
		pkt := &mysql.StatisticsPacket{
			Command: payloadType,
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STATISTICS), clientConn, mysql.COM_STATISTICS, decodeCtx)

	case payloadType == mysql.COM_DEBUG:
		logger.Debug("COM_DEBUG packet", zap.Any("Type", payloadType))
		pkt := &mysql.DebugPacket{
			Command: payloadType,
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_DEBUG), clientConn, mysql.COM_DEBUG, decodeCtx)

	case payloadType == mysql.COM_PING:
		logger.Debug("COM_PING packet", zap.Any("Type", payloadType))
		pkt := &mysql.PingPacket{
			Command: payloadType,
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_PING), clientConn, mysql.COM_PING, decodeCtx)

	case payloadType == mysql.COM_CHANGE_USER:
		logger.Debug("COM_CHANGE_USER packet", zap.Any("Type", payloadType))
		pkt := &mysql.ChangeUserPacket{
			Command: payloadType,
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_CHANGE_USER), clientConn, mysql.COM_CHANGE_USER, decodeCtx)

	case payloadType == mysql.COM_RESET_CONNECTION:
		logger.Debug("COM_RESET_CONNECTION packet", zap.Any("Type", payloadType))
		pkt := &mysql.ResetConnectionPacket{
			Command: payloadType,
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION), clientConn, mysql.COM_RESET_CONNECTION, decodeCtx)

	// case payloadType == mysql.COM_SET_OPTION:
	// 	logger.Debug("COM_SET_OPTION packet", zap.Any("Type", payloadType))
	// 	pkt, err := utility.DecodeSetOption(ctx, payload)
	// 	if err != nil {
	// 		return parsedPacket, fmt.Errorf("failed to decode COM_SET_OPTION packet: %w", err)
	// 	}

	// 	setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_SET_OPTION), mysql.COM_SET_OPTION, decodeCtx)

	// command packets
	case payloadType == mysql.COM_QUERY:
		//debug log
		logger.Info("COM_QUERY packet", zap.Any("Type", payloadType))

		pkt, err := command.DecodeQuery(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_QUERY packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_QUERY), clientConn, mysql.COM_QUERY, decodeCtx)
		lstOp, _ := decodeCtx.LastOp.Load(clientConn)
		println("lastOp", lstOp)

	case payloadType == mysql.COM_STMT_PREPARE:
		//debug log
		logger.Info("COM_STMT_PREPARE packet", zap.Any("Type", payloadType))

		pkt, err := preparedstmt.DecodeStmtPrepare(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_PREPARE packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_PREPARE), clientConn, mysql.COM_STMT_PREPARE, decodeCtx)

	case payloadType == mysql.COM_STMT_EXECUTE:
		//debug log
		logger.Info("COM_STMT_EXECUTE packet", zap.Any("Type", payloadType))
		pkt, err := preparedstmt.DecodeStmtExecute(ctx, logger, payload, decodeCtx.PreparedStatements)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_EXECUTE packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE), clientConn, mysql.COM_STMT_EXECUTE, decodeCtx)

	// case payloadType == mysql.COM_STMT_FETCH:
	case payloadType == mysql.COM_STMT_CLOSE:
		if len(payload) > 9 {
			if payload[9] == mysql.COM_STMT_PREPARE {
				logger.Debug("COM_STMT_CLOSE_WITH_PREPARE packet", zap.Any("Type", payloadType))
				pkt, err := preparedstmt.DecodeCloseAndPrepare(ctx, payload)
				if err != nil {
					return parsedPacket, fmt.Errorf("failed to decode COM_STMT_CLOSE_WITH_PREPARE packet: %w", err)
				}

				setPacketInfo(ctx, parsedPacket, pkt, "COM_STMT_CLOSE_WITH_PREPARE", clientConn, mysql.COM_STMT_PREPARE, decodeCtx)
			} else if payload[9] == mysql.COM_QUERY {
				logger.Debug("COM_STMT_CLOSE_WITH_QUERY packet", zap.Any("Type", payloadType))
				pkt, err := preparedstmt.DecodeCloseAndQuery(ctx, payload)
				if err != nil {
					return parsedPacket, fmt.Errorf("failed to decode COM_STMT_CLOSE_WITH_QUERY packet: %w", err)
				}

				setPacketInfo(ctx, parsedPacket, pkt, "COM_STMT_CLOSE_WITH_QUERY", clientConn, mysql.COM_QUERY, decodeCtx)
			}
		} else {
			logger.Debug("COM_STMT_CLOSE packet", zap.Any("Type", payloadType))
			pkt, err := preparedstmt.DecoderStmtClose(ctx, payload)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode COM_STMT_CLOSE packet: %w", err)
			}

			setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_CLOSE), clientConn, mysql.COM_STMT_CLOSE, decodeCtx)
		}

	case payloadType == mysql.COM_STMT_RESET:
		logger.Debug("COM_STMT_RESET packet", zap.Any("Type", payloadType))
		pkt, err := preparedstmt.DecodeStmtReset(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_RESET packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_RESET), clientConn, mysql.COM_STMT_RESET, decodeCtx)

	case payloadType == mysql.COM_STMT_SEND_LONG_DATA:
		logger.Debug("COM_STMT_SEND_LONG_DATA packet", zap.Any("Type", payloadType))
		pkt, err := preparedstmt.DecodeStmtSendLongData(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_SEND_LONG_DATA packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA), clientConn, mysql.COM_STMT_SEND_LONG_DATA, decodeCtx)

	default:
		logger.Warn("Unknown packet type", zap.String("PacketType", fmt.Sprintf("%#x", payloadType)), zap.Any("payload", payload), zap.Any("last operation", lastOp))
		setPacketInfo(ctx, parsedPacket, payload, "Unknown type", clientConn, RESET, decodeCtx)
		//print the entire payload in hexademical format
		// printByteArray("Unknown type", payload)
	}

	if decodeCtx.Mode == models.MODE_TEST {
		decodeCtx.LastOp.Store(clientConn, RESET) //reset the last operation
	}

	return parsedPacket, nil
}

// PrintByteArray is only for debugging purpose
func PrintByteArray(name string, b []byte) {
	fmt.Printf("%s:\n", name)
	var i = 1
	for _, byte := range b {
		fmt.Printf(" %02x", byte)
		i++
		if i%16 == 0 {
			fmt.Println()
		}
	}
	fmt.Println()
}
