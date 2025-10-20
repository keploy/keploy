package wire

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/query/preparedstmt"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

func EncodeToBinary(ctx context.Context, logger *zap.Logger, packet *mysql.PacketBundle, clientConn net.Conn, decodeCtx *DecodeContext) ([]byte, error) {

	var data []byte
	var err error

	//Get the server greeting from the decode context
	serverGreeting, ok := decodeCtx.ServerGreetings.Load(clientConn)
	if !ok {
		return nil, fmt.Errorf("server greeting not found for connection %s", clientConn.RemoteAddr().String())
	}

	switch packet.Message.(type) {
	// generic response packets
	case *mysql.EOFPacket:
		pkt, ok := packet.Message.(*mysql.EOFPacket)
		if !ok {
			return nil, fmt.Errorf("expected EOFPacket, got %T", packet.Message)
		}

		data, err = phase.EncodeEOF(ctx, pkt, serverGreeting.CapabilityFlags)
		if err != nil {
			return nil, fmt.Errorf("error encoding EOF packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.EOF)

	case *mysql.ERRPacket:
		pkt, ok := packet.Message.(*mysql.ERRPacket)
		if !ok {
			return nil, fmt.Errorf("expected ERRPacket, got %T", packet.Message)
		}

		data, err = phase.EncodeErr(ctx, pkt, serverGreeting.CapabilityFlags)
		if err != nil {
			return nil, fmt.Errorf("error encoding ERR packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.ERR)

	case *mysql.OKPacket:
		pkt, ok := packet.Message.(*mysql.OKPacket)
		if !ok {
			return nil, fmt.Errorf("expected OKPacket, got %T", packet.Message)
		}

		data, err = phase.EncodeOk(ctx, pkt, serverGreeting.CapabilityFlags)
		if err != nil {
			return nil, fmt.Errorf("error encoding OK packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.OK)

	// connection phase packets
	case *mysql.AuthMoreDataPacket:
		pkt, ok := packet.Message.(*mysql.AuthMoreDataPacket)
		if !ok {
			return nil, fmt.Errorf("expected AuthMoreDataPacket, got %T", packet.Message)
		}

		data, err = conn.EncodeAuthMoreData(ctx, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding AuthMoreData packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.AuthMoreData)

	case *mysql.AuthSwitchRequestPacket:
		pkt, ok := packet.Message.(*mysql.AuthSwitchRequestPacket)
		if !ok {
			return nil, fmt.Errorf("expected AuthSwitchRequestPacket, got %T", packet.Message)
		}

		data, err = conn.EncodeAuthSwitchRequest(ctx, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding AuthSwitchRequest packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.AuthSwitchRequest)

	case *mysql.HandshakeV10Packet:
		pkt, ok := packet.Message.(*mysql.HandshakeV10Packet)
		if !ok {
			return nil, fmt.Errorf("expected HandshakeV10Packet, got %T", packet.Message)
		}

		data, err = conn.EncodeHandshakeV10(ctx, logger, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding HandshakeV10 packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)

	// command phase packets
	case *mysql.StmtPrepareOkPacket:
		pkt, ok := packet.Message.(*mysql.StmtPrepareOkPacket)
		if !ok {
			return nil, fmt.Errorf("expected StmtPrepareOkPacket, got %T", packet.Message)
		}

		data, err = preparedstmt.EncodePrepareOk(ctx, logger, pkt, serverGreeting.CapabilityFlags)
		if err != nil {
			return nil, fmt.Errorf("error encoding StmtPrepareOkPacket: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.COM_STMT_PREPARE)

	case *mysql.TextResultSet:
		pkt, ok := packet.Message.(*mysql.TextResultSet)
		if !ok {
			return nil, fmt.Errorf("expected TextResultSet, got %T", packet.Message)
		}

		data, err = query.EncodeTextResultSet(ctx, logger, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding TextResultSet: %v", err)
		}

	case *mysql.BinaryProtocolResultSet:
		pkt, ok := packet.Message.(*mysql.BinaryProtocolResultSet)
		if !ok {
			return nil, fmt.Errorf("expected BinaryProtocolResultSet, got %T", packet.Message)
		}

		data, err = query.EncodeBinaryResultSet(ctx, logger, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding BinaryProtocolResultSet: %v", err)
		}
	}

	// Encode the header for the packet
	header := make([]byte, 4)
	// IMPORTANT:
	// For composite (multi-packet) messages, `data` contains multiple packets where
	// only the FIRST sub-packet lacks its 4-byte header. The recorded header's
	// PayloadLength corresponds to that first payload only (e.g., column-count = 1).
	// For single-packet messages, we can safely use len(data).
	payloadLen := len(data)
	if isCompositeMessage(packet.Message) && packet.Header != nil && packet.Header.Header != nil {
		payloadLen = int(packet.Header.Header.PayloadLength)
	}
	binary.LittleEndian.PutUint32(header, uint32(payloadLen))
	header[3] = packet.Header.Header.SequenceID
	data = append(header, data...)

	logger.Debug("Encoded Packet", zap.String("packet", packet.Header.Type), zap.ByteString("data", data))

	return data, nil
}

// isCompositeMessage tells whether the encoded `data` contains multiple packets,
// where only the first packet should use the header we write here.
func isCompositeMessage(msg interface{}) bool {
	switch msg.(type) {
	case *mysql.TextResultSet,
		*mysql.BinaryProtocolResultSet,
		*mysql.StmtPrepareOkPacket:
		return true
	default:
		return false
	}
}
