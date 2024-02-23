package mysql

import (
	"context"
	"encoding/binary"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"log"
	"net"
)

// TODO:Remove these global variables, and find a better way to handle this if possible
var (
	isPluginData                   = false
	expectingAuthSwitchResponse    = false
	expectingHandshakeResponse     = false
	expectingHandshakeResponseTest = false
)

func bytesToMySQLPacket(buffer []byte) MySQLPacket {
	if buffer == nil || len(buffer) < 4 {
		log.Fatalf("Error: buffer is nil or too short to be a valid MySQL packet")
		return MySQLPacket{}
	}
	tempBuffer := make([]byte, 4)
	copy(tempBuffer, buffer[:3])
	length := binary.LittleEndian.Uint32(tempBuffer)
	sequenceID := buffer[3]
	payload := buffer[4:]
	return MySQLPacket{
		Header: MySQLPacketHeader{
			PayloadLength: length,
			SequenceID:    sequenceID,
		},
		Payload: payload,
	}
}

func readFirstBuffer(ctx context.Context, clientConn, destConn net.Conn) ([]byte, string, error) {
	// Attempt to read from destConn first
	buf, err := util.ReadBytes(ctx, destConn)
	// If there is data from destConn, return it
	if err == nil {
		return buf, "destination", nil
	}
	// If the error is a timeout, try to read from clientConn
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		buf, err = util.ReadBytes(ctx, clientConn)
		// If there is data from clientConn, return it
		if err == nil {
			return buf, "client", nil
		}
		// Return any error from reading clientConn
		return nil, "", err
	}
	// Return any other error from reading destConn
	return nil, "", err
}
