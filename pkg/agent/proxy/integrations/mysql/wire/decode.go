// Package wire provides encoding and decoding operation of MySQL packets.
package wire

import (
	"context"
	"fmt"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	connection "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/preparedstmt"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/utility"

	itgUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// DecodePayload is used to decode mysql packets that don't consist of multiple packets within them, because we are reading per packet.
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
		if ce := logger.Check(zapcore.DebugLevel, "Last operation not found in DecodePayload"); ce != nil {
			ce.Write()
		}
		lastOp = 0x00
	}

	if ce := logger.Check(zapcore.DebugLevel, "DecodePayload"); ce != nil {
		ce.Write(zap.Uint8("lastOp", lastOp), zap.String("mode", string(decodeCtx.Mode)))
	}

	if (lastOp == mysql.COM_QUERY || lastOp == mysql.COM_STMT_EXECUTE) && decodeCtx.Mode == models.MODE_RECORD {
		return handleQueryStmtResponse(ctx, logger, packet, clientConn, lastOp, decodeCtx)
	}

	parsedPacket, err := decodePacket(ctx, logger, packet, clientConn, lastOp, decodeCtx)
	if err != nil {
		return &mysql.PacketBundle{}, fmt.Errorf("failed to decode packet: %w", err)
	}

	return parsedPacket, nil
}

// DecodePayloadFast is the hot-path version of DecodePayload for the command
// phase.  It reads lastOp and server greeting from the direct DecodeContext
// fields (LastOpValue / ServerGreeting) instead of going through map lookups
// with RWMutex — eliminating 2-4 mutex acquire/release pairs per packet.
//
// Use this ONLY when DecodeContext.LastOpValue and .ServerGreeting have been
// populated (i.e., after the handshake phase).
func DecodePayloadFast(ctx context.Context, logger *zap.Logger, data []byte, decodeCtx *DecodeContext) (*mysql.PacketBundle, error) {
	packet, err := utils.BytesToMySQLPacket(data)
	if err != nil {
		return &mysql.PacketBundle{}, fmt.Errorf("failed to parse MySQL packet: %w", err)
	}

	if len(packet.Payload) < 1 {
		return &mysql.PacketBundle{}, fmt.Errorf("invalid packet, payload is empty")
	}

	lastOp := decodeCtx.LastOpValue

	if ce := logger.Check(zapcore.DebugLevel, "DecodePayloadFast"); ce != nil {
		ce.Write(zap.Uint8("lastOp", lastOp), zap.String("mode", string(decodeCtx.Mode)))
	}

	if (lastOp == mysql.COM_QUERY || lastOp == mysql.COM_STMT_EXECUTE) && decodeCtx.Mode == models.MODE_RECORD {
		return handleQueryStmtResponseFast(ctx, logger, packet, lastOp, decodeCtx)
	}

	parsedPacket, err := decodePacketFast(ctx, logger, packet, lastOp, decodeCtx)
	if err != nil {
		// Return the partially-parsed packet so callers still get a valid
		// Header (non-nil).  Previous code returned &mysql.PacketBundle{}
		// which left Header nil and caused nil-pointer panics in callers
		// that accessed decoded.Header.Type.
		return parsedPacket, fmt.Errorf("failed to decode packet: %w", err)
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
		return parsedPacket, fmt.Errorf("server Greetings not found")
	}

	switch payloadType {
	case mysql.OK:
		pkt, err := phase.DecodeOk(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode OK packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.OK), clientConn, RESET, decodeCtx)

	case mysql.ERR:

		pkt, err := phase.DecodeERR(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode ERR packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.ERR), clientConn, RESET, decodeCtx)

	case mysql.EOF:
		pkt, err := phase.DecodeEOF(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode EOF packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.EOF), clientConn, RESET, decodeCtx)

	case mysql.LocalInFile:
		parsedPacket.Header.Type = "LocalInFile"
		decodeCtx.LastOp.Store(clientConn, RESET) //reset the last operation
		return parsedPacket, fmt.Errorf("LocalInFile not supported")
	default:
		//If the packet is not OK, ERR, EOF or LocalInFile, then it is a result set
		var pktType string
		var rowType query.RowType
		if lastOp == mysql.COM_STMT_EXECUTE {
			rowType = query.Binary
			pktType = string(mysql.Binary)
		} else {
			rowType = query.Text
			pktType = string(mysql.Text)
		}

		pkt, err := query.DecodeResultSetMetadata(ctx, logger, payload, rowType)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode result set: %w", err)
		}

		// Do not change the last operation if the packet is a result set, it will be changed when the result set is fully received
		setPacketInfo(ctx, parsedPacket, pkt, pktType, clientConn, lastOp, decodeCtx)
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
			return parsedPacket, fmt.Errorf("server Greetings not found")
		}
	}

	// Handle handshakeResponse41 separately, because its status is not defined and can be changed with the client capabilities.
	if lastOp == mysql.HandshakeV10 {
		pkt, err := connection.DecodeHandshakeResponse(ctx, logger, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode HandshakeResponse41 packet: %w", err)
		}

		var pktType string
		switch pkt := pkt.(type) {
		case *mysql.HandshakeResponse41Packet:
			// Store the client capabilities to use it later
			decodeCtx.ClientCapabilities = pkt.CapabilityFlags

			pktType = mysql.HandshakeResponse41
			lastOp = payloadType
		case *mysql.SSLRequestPacket:
			// Store the client capabilities to use it later
			decodeCtx.ClientCapabilities = pkt.CapabilityFlags

			pktType = mysql.SSLRequest
			decodeCtx.UseSSL = true
			// Don't change the last operation if the packet is an SSL Request
		}

		setPacketInfo(ctx, parsedPacket, pkt, pktType, clientConn, lastOp, decodeCtx)

		return parsedPacket, nil
	}

	switch {
	// generic response packets
	case payloadType == mysql.EOF && len(payload) == 5: //assuming that the payload is always 5 bytes
		pkt, err := phase.DecodeEOF(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode EOF packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.EOF), clientConn, mysql.EOF, decodeCtx)

	case payloadType == mysql.ERR:
		pkt, err := phase.DecodeERR(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode ERR packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.ERR), clientConn, mysql.ERR, decodeCtx)

	case payloadType == mysql.OK:
		if lastOp == mysql.COM_STMT_PREPARE {
			pkt, err := preparedstmt.DecodePrepareOk(ctx, logger, payload, sg.CapabilityFlags)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode COM_STMT_PREPARE_OK packet: %w", err)
			}

			// Do not change the last operation if the packet is a prepared statement, it will be changed when the prepared statement is fully received
			setPacketInfo(ctx, parsedPacket, pkt, "COM_STMT_PREPARE_OK", clientConn, lastOp, decodeCtx)
			// Store the prepared statement to use it later
			decodeCtx.PreparedStatements[pkt.StatementID] = pkt

		} else {
			pkt, err := phase.DecodeOk(ctx, payload, sg.CapabilityFlags)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode OK packet: %w", err)
			}

			setPacketInfo(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.OK), clientConn, mysql.OK, decodeCtx)
		}

		// auth packets
	case payloadType == 0x01:
		if len(payload) == 1 {
			pkt := &mysql.QuitPacket{
				Command: payloadType,
			}
			setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_QUIT), clientConn, mysql.COM_QUIT, decodeCtx)
		} else {
			//otherwise it is a AuthMoreData packet
			pkt, err := connection.DecodeAuthMoreData(ctx, payload)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode AuthMoreData packet: %w", err)
			}
			setPacketInfo(ctx, parsedPacket, pkt, mysql.AuthStatusToString(mysql.AuthMoreData), clientConn, mysql.AuthMoreData, decodeCtx)
		}
	case payloadType == mysql.AuthSwitchRequest && len(payload) > 5: //conflicting with EOF packet, assuming that the payload is always greater than 5 bytes
		pkt, err := connection.DecodeAuthSwitchRequest(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode AuthSwitchRequest packet: %w", err)
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.AuthStatusToString(mysql.AuthSwitchRequest), clientConn, mysql.AuthSwitchRequest, decodeCtx)

	case payloadType == 0x02:
		// 0x02 is ambiguous: it can be COM_INIT_DB or auth-related (AuthNextFactor / request public key).
		// Resolve based on the last observed auth phase.
		if lastOp == mysql.AuthMoreData || lastOp == mysql.AuthSwitchRequest || lastOp == mysql.HandshakeV10 || lastOp == mysql.AuthNextFactor {
			if len(payload) == 1 {
				setPacketInfo(ctx, parsedPacket, "request_public_key", mysql.CachingSha2PasswordToString(mysql.RequestPublicKey), clientConn, byte(mysql.RequestPublicKey), decodeCtx)
			} else {
				pkt, err := connection.DecodeAuthNextFactor(ctx, payload)
				if err != nil {
					return parsedPacket, fmt.Errorf("failed to decode AuthNextFactor packet: %w", err)
				}
				logger.Warn("AuthNextFactor packet not supported, further flow can be affected")
				setPacketInfo(ctx, parsedPacket, pkt, mysql.AuthStatusToString(mysql.AuthNextFactor), clientConn, mysql.AuthNextFactor, decodeCtx)
			}
		} else {
			pkt, err := utility.DecodeInitDb(ctx, payload)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode COM_INIT_DB packet: %w", err)
			}

			setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_INIT_DB), clientConn, mysql.COM_INIT_DB, decodeCtx)
		}
	case payloadType == mysql.HandshakeV10:
		pkt, err := connection.DecodeHandshakeV10(ctx, logger, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode HandshakeV10 packet: %w", err)
		}
		// Store the server greetings to use it later
		decodeCtx.ServerGreetings.Store(clientConn, pkt)
		setPacketInfo(ctx, parsedPacket, pkt, mysql.AuthStatusToString(mysql.HandshakeV10), clientConn, mysql.HandshakeV10, decodeCtx)

		// utility packets
	case payloadType == mysql.COM_QUIT:
		pkt := &mysql.QuitPacket{
			Command: payloadType,
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_QUIT), clientConn, mysql.COM_QUIT, decodeCtx)
	case payloadType == mysql.COM_STATISTICS:
		pkt := &mysql.StatisticsPacket{
			Command: payloadType,
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STATISTICS), clientConn, mysql.COM_STATISTICS, decodeCtx)

	case payloadType == mysql.COM_DEBUG:
		pkt := &mysql.DebugPacket{
			Command: payloadType,
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_DEBUG), clientConn, mysql.COM_DEBUG, decodeCtx)

	case payloadType == mysql.COM_PING:
		pkt := &mysql.PingPacket{
			Command: payloadType,
		}
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_PING), clientConn, mysql.COM_PING, decodeCtx)

	case payloadType == mysql.COM_CHANGE_USER:
		pkt := &mysql.ChangeUserPacket{
			Command: payloadType,
		}
		logger.Warn("COM_CHANGE_USER packet not supported, further flow can be affected")
		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_CHANGE_USER), clientConn, mysql.COM_CHANGE_USER, decodeCtx)

	case payloadType == mysql.COM_RESET_CONNECTION:
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
		pkt, err := query.DecodeQuery(ctx, logger, payload, decodeCtx.ClientCapabilities)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_QUERY packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_QUERY), clientConn, mysql.COM_QUERY, decodeCtx)

	case payloadType == mysql.COM_STMT_PREPARE:
		pkt, err := preparedstmt.DecodeStmtPrepare(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_PREPARE packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_PREPARE), clientConn, mysql.COM_STMT_PREPARE, decodeCtx)

	case payloadType == mysql.COM_STMT_EXECUTE:
		pkt, err := preparedstmt.DecodeStmtExecute(ctx, logger, payload, decodeCtx.PreparedStatements, decodeCtx.ClientCapabilities)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_EXECUTE packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE), clientConn, mysql.COM_STMT_EXECUTE, decodeCtx)

	// case payloadType == mysql.COM_STMT_FETCH:
	case payloadType == mysql.COM_STMT_CLOSE:
		pkt, err := preparedstmt.DecoderStmtClose(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_CLOSE packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_CLOSE), clientConn, mysql.COM_STMT_CLOSE, decodeCtx)

	case payloadType == mysql.COM_STMT_RESET:
		pkt, err := preparedstmt.DecodeStmtReset(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_RESET packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_RESET), clientConn, mysql.COM_STMT_RESET, decodeCtx)

	case payloadType == mysql.COM_STMT_SEND_LONG_DATA:
		pkt, err := preparedstmt.DecodeStmtSendLongData(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_SEND_LONG_DATA packet: %w", err)
		}

		setPacketInfo(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA), clientConn, mysql.COM_STMT_SEND_LONG_DATA, decodeCtx)
	default:
		logger.Debug("Unknown packet type", zap.String("PacketType", fmt.Sprintf("%#x", payloadType)), zap.Uint8("lastOp", lastOp))
		setPacketInfo(ctx, parsedPacket, itgUtils.EncodeBase64(payload), fmt.Sprintf("%#x", payloadType), clientConn, RESET, decodeCtx)
	}

	return parsedPacket, nil
}

// ── Fast-path variants (no map/mutex, use DecodeContext direct fields) ────

// handleQueryStmtResponseFast is the hot-path version of handleQueryStmtResponse.
// It uses decodeCtx.ServerGreeting directly instead of the ServerGreetings map.
func handleQueryStmtResponseFast(ctx context.Context, logger *zap.Logger, packet mysql.Packet, lastOp byte, decodeCtx *DecodeContext) (*mysql.PacketBundle, error) {
	header := packet.Header
	payload := packet.Payload

	parsedPacket := &mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &header,
		},
	}

	payloadType := payload[0]

	sg := decodeCtx.ServerGreeting
	if sg == nil {
		return parsedPacket, fmt.Errorf("server Greetings not found (fast path)")
	}

	switch payloadType {
	case mysql.OK:
		pkt, err := phase.DecodeOk(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode OK packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.OK), RESET, decodeCtx)

	case mysql.ERR:
		pkt, err := phase.DecodeERR(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode ERR packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.ERR), RESET, decodeCtx)

	case mysql.EOF:
		pkt, err := phase.DecodeEOF(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode EOF packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.EOF), RESET, decodeCtx)

	case mysql.LocalInFile:
		parsedPacket.Header.Type = "LocalInFile"
		decodeCtx.LastOpValue = RESET
		return parsedPacket, fmt.Errorf("LocalInFile not supported")

	default:
		var pktType string
		var rowType query.RowType
		if lastOp == mysql.COM_STMT_EXECUTE {
			rowType = query.Binary
			pktType = string(mysql.Binary)
		} else {
			rowType = query.Text
			pktType = string(mysql.Text)
		}

		pkt, err := query.DecodeResultSetMetadata(ctx, logger, payload, rowType)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode result set: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, pktType, lastOp, decodeCtx)
	}

	return parsedPacket, nil
}

// decodePacketFast is the hot-path version of decodePacket.
// It uses decodeCtx.ServerGreeting and decodeCtx.LastOpValue directly.
func decodePacketFast(ctx context.Context, logger *zap.Logger, packet mysql.Packet, lastOp byte, decodeCtx *DecodeContext) (*mysql.PacketBundle, error) {
	header := packet.Header
	payload := packet.Payload

	parsedPacket := &mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &header,
		},
	}

	payloadType := payload[0]

	sg := decodeCtx.ServerGreeting
	// No need to find the server greetings if the payload is HandshakeV10
	if payloadType != mysql.HandshakeV10 && sg == nil {
		return parsedPacket, fmt.Errorf("server Greetings not found (fast path)")
	}

	// Command-phase packets only (no handshake handling needed on fast path)
	switch {
	case payloadType == mysql.OK:
		if lastOp == mysql.COM_STMT_PREPARE {
			pkt, err := preparedstmt.DecodePrepareOk(ctx, logger, payload, sg.CapabilityFlags)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode COM_STMT_PREPARE_OK packet: %w", err)
			}
			SetPacketInfoFast(ctx, parsedPacket, pkt, "COM_STMT_PREPARE_OK", lastOp, decodeCtx)
			decodeCtx.PreparedStatements[pkt.StatementID] = pkt
		} else {
			pkt, err := phase.DecodeOk(ctx, payload, sg.CapabilityFlags)
			if err != nil {
				return parsedPacket, fmt.Errorf("failed to decode OK packet: %w", err)
			}
			SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.OK), mysql.OK, decodeCtx)
		}

	case payloadType == mysql.ERR:
		pkt, err := phase.DecodeERR(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode ERR packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.ERR), mysql.ERR, decodeCtx)

	case payloadType == mysql.EOF && len(payload) == 5:
		pkt, err := phase.DecodeEOF(ctx, payload, sg.CapabilityFlags)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode EOF packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.StatusToString(mysql.EOF), mysql.EOF, decodeCtx)

	case payloadType == mysql.COM_QUIT:
		pkt := &mysql.QuitPacket{Command: payloadType}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_QUIT), mysql.COM_QUIT, decodeCtx)

	case payloadType == mysql.COM_QUERY:
		pkt, err := query.DecodeQuery(ctx, logger, payload, decodeCtx.ClientCapabilities)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_QUERY packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_QUERY), mysql.COM_QUERY, decodeCtx)

	case payloadType == mysql.COM_STMT_PREPARE:
		pkt, err := preparedstmt.DecodeStmtPrepare(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_PREPARE packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_PREPARE), mysql.COM_STMT_PREPARE, decodeCtx)

	case payloadType == mysql.COM_STMT_EXECUTE:
		pkt, err := preparedstmt.DecodeStmtExecute(ctx, logger, payload, decodeCtx.PreparedStatements, decodeCtx.ClientCapabilities)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_EXECUTE packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_EXECUTE), mysql.COM_STMT_EXECUTE, decodeCtx)

	case payloadType == mysql.COM_STMT_CLOSE:
		pkt, err := preparedstmt.DecoderStmtClose(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_CLOSE packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_CLOSE), mysql.COM_STMT_CLOSE, decodeCtx)

	case payloadType == mysql.COM_STMT_RESET:
		pkt, err := preparedstmt.DecodeStmtReset(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_RESET packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_RESET), mysql.COM_STMT_RESET, decodeCtx)

	case payloadType == mysql.COM_STMT_SEND_LONG_DATA:
		pkt, err := preparedstmt.DecodeStmtSendLongData(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_STMT_SEND_LONG_DATA packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STMT_SEND_LONG_DATA), mysql.COM_STMT_SEND_LONG_DATA, decodeCtx)

	case payloadType == mysql.COM_PING:
		pkt := &mysql.PingPacket{Command: payloadType}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_PING), mysql.COM_PING, decodeCtx)

	case payloadType == mysql.COM_STATISTICS:
		pkt := &mysql.StatisticsPacket{Command: payloadType}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_STATISTICS), mysql.COM_STATISTICS, decodeCtx)

	case payloadType == mysql.COM_DEBUG:
		pkt := &mysql.DebugPacket{Command: payloadType}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_DEBUG), mysql.COM_DEBUG, decodeCtx)

	case payloadType == mysql.COM_RESET_CONNECTION:
		pkt := &mysql.ResetConnectionPacket{Command: payloadType}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_RESET_CONNECTION), mysql.COM_RESET_CONNECTION, decodeCtx)

	case payloadType == mysql.COM_CHANGE_USER:
		pkt := &mysql.ChangeUserPacket{Command: payloadType}
		logger.Warn("COM_CHANGE_USER packet not supported, further flow can be affected")
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_CHANGE_USER), mysql.COM_CHANGE_USER, decodeCtx)

	case payloadType == mysql.COM_INIT_DB:
		pkt, err := utility.DecodeInitDb(ctx, payload)
		if err != nil {
			return parsedPacket, fmt.Errorf("failed to decode COM_INIT_DB packet: %w", err)
		}
		SetPacketInfoFast(ctx, parsedPacket, pkt, mysql.CommandStatusToString(mysql.COM_INIT_DB), mysql.COM_INIT_DB, decodeCtx)

	default:
		logger.Debug("Unknown packet type (fast path)", zap.String("PacketType", fmt.Sprintf("%#x", payloadType)), zap.Uint8("lastOp", lastOp))
		SetPacketInfoFast(ctx, parsedPacket, itgUtils.EncodeBase64(payload), fmt.Sprintf("%#x", payloadType), RESET, decodeCtx)
	}

	return parsedPacket, nil
}
