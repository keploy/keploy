//go:build linux

package operation

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/command/preparedstmt"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/connection"
	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/generic"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

/*
    1.  MySQLStructToBytes
	2.	EncodeMySQLStruct
	3.	MySQLPacketToBytes
	4.	MarshalMySQLPacket
	5.	ConvertMySQLToBytes
	6.	SerializeMySQLPacket
	7.	EncodeMySQLData
	8.	MySQLDataToBytes
	9.	PackMySQLBytes
	10.	StructToMySQLBytes
*/

func EncodeToBinary(ctx context.Context, logger *zap.Logger, packet *mysql.PacketBundle, clientConn net.Conn, decodeCtx *DecodeContext) ([]byte, error) {

	var data []byte
	var err error

	//Get the server greeting from the decode context
	serverGreeting, ok := decodeCtx.ServerGreetings.Load(clientConn)
	if !ok {
		return nil, fmt.Errorf("Server greeting not found for connection %s", clientConn.RemoteAddr().String())
	}

	switch packet.Message.(type) {
	// generic response packets
	case *mysql.EOFPacket:
		pkt, ok := packet.Message.(*mysql.EOFPacket)
		if !ok {
			return nil, fmt.Errorf("Expected EOFPacket, got %T", packet.Message)
		}

		data, err = generic.EncodeEOF(ctx, pkt, serverGreeting.CapabilityFlags)
		if err != nil {
			return nil, fmt.Errorf("error encoding EOF packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.EOF)

	case *mysql.ERRPacket:
		pkt, ok := packet.Message.(*mysql.ERRPacket)
		if !ok {
			return nil, fmt.Errorf("Expected ERRPacket, got %T", packet.Message)
		}

		data, err = generic.EncodeErr(ctx, pkt, serverGreeting.CapabilityFlags)
		if err != nil {
			return nil, fmt.Errorf("error encoding ERR packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.ERR)

	case *mysql.OKPacket:
		pkt, ok := packet.Message.(*mysql.OKPacket)
		if !ok {
			return nil, fmt.Errorf("Expected OKPacket, got %T", packet.Message)
		}

		data, err = generic.EncodeOk(ctx, pkt, serverGreeting.CapabilityFlags)
		if err != nil {
			return nil, fmt.Errorf("error encoding OK packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.OK)

	// connection phase packets
	case *mysql.AuthMoreDataPacket:
		pkt, ok := packet.Message.(*mysql.AuthMoreDataPacket)
		if !ok {
			return nil, fmt.Errorf("Expected AuthMoreDataPacket, got %T", packet.Message)
		}

		data, err = connection.EncodeAuthMoreData(ctx, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding AuthMoreData packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.AuthMoreData)

	case *mysql.AuthSwitchRequestPacket:
		pkt, ok := packet.Message.(*mysql.AuthSwitchRequestPacket)
		if !ok {
			return nil, fmt.Errorf("Expected AuthSwitchRequestPacket, got %T", packet.Message)
		}

		data, err = connection.EncodeAuthSwitchRequest(ctx, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding AuthSwitchRequest packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.AuthSwitchRequest)

	case *mysql.HandshakeV10Packet:
		pkt, ok := packet.Message.(*mysql.HandshakeV10Packet)
		if !ok {
			return nil, fmt.Errorf("Expected HandshakeV10Packet, got %T", packet.Message)
		}

		data, err = connection.EncodeHandshakeV10(ctx, logger, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding HandshakeV10 packet: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.HandshakeV10)

	// command phase packets
	case *mysql.StmtPrepareOkPacket:
		pkt, ok := packet.Message.(*mysql.StmtPrepareOkPacket)
		if !ok {
			return nil, fmt.Errorf("Expected StmtPrepareOkPacket, got %T", packet.Message)
		}

		data, err = preparedstmt.EncodePrepareOk(ctx, logger, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding StmtPrepareOkPacket: %v", err)
		}

		decodeCtx.LastOp.Store(clientConn, mysql.COM_STMT_PREPARE)

	case *mysql.TextResultSet:
		pkt, ok := packet.Message.(*mysql.TextResultSet)
		if !ok {
			return nil, fmt.Errorf("Expected TextResultSet, got %T", packet.Message)
		}

		data, err = command.EncodeTextResultSet(ctx, logger, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding TextResultSet: %v", err)
		}

	case *mysql.BinaryProtocolResultSet:
		pkt, ok := packet.Message.(*mysql.BinaryProtocolResultSet)
		if !ok {
			return nil, fmt.Errorf("Expected BinaryProtocolResultSet, got %T", packet.Message)
		}

		data, err = command.EncodeBinaryResultSet(ctx, logger, pkt)
		if err != nil {
			return nil, fmt.Errorf("error encoding BinaryProtocolResultSet: %v", err)
		}
	}

	// Encode the header for the packet
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, uint32(len(data)))
	header[3] = packet.Header.Header.SequenceID
	data = append(header, data...)

	logger.Debug("Encoded Packet", zap.String("packet", packet.Header.Type), zap.ByteString("data", data))

	return data, nil
}
