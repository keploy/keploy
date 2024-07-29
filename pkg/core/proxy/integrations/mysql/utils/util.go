//go:build linux

// Package utils provides utility functions for MySQL packets
package utils

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.uber.org/zap"
)

// ReadFirstBuffer reads the first buffer from either clientConn or destConn
func ReadFirstBuffer(ctx context.Context, logger *zap.Logger, clientConn, destConn net.Conn) ([]byte, string, error) {
	// Attempt to read from destConn first
	buf, err := util.ReadBytes(ctx, logger, destConn)
	// If there is data from destConn, return it
	if err == nil {
		return buf, "destination", nil
	}
	// If the error is a timeout, try to read from clientConn
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		buf, err = util.ReadBytes(ctx, logger, clientConn)
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

// BytesToMySQLPacket converts a byte slice to a MySQL packet
func BytesToMySQLPacket(buffer []byte) (mysql.Packet, error) {
	if buffer == nil || len(buffer) < 4 {
		return mysql.Packet{}, errors.New("buffer is nil or too short to be a valid MySQL packet")
	}

	tempBuffer := make([]byte, 4)
	copy(tempBuffer, buffer[:3])
	length := binary.LittleEndian.Uint32(tempBuffer)
	sequenceID := buffer[3]

	payload := buffer[4:]

	return mysql.Packet{
		Header: mysql.Header{
			PayloadLength: length,
			SequenceID:    sequenceID,
		},
		Payload: payload,
	}, nil
}

// GetPayloadLength returns the length of the payload from the first 3 bytes of the packet.
func GetPayloadLength(src []byte) (length int32) {
	length = int32(src[0]) | int32(src[1])<<8 | int32(src[2])<<16
	return length
}

func ReadLengthEncodedInteger(b []byte) (num uint64, isNull bool, n int) {
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

func IsEOFPacket(data []byte) bool {
	return len(data) > 4 && bytes.Contains(data[4:9], []byte{0xfe, 0x00, 0x00})
}

func ReadUint24(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
}

func ReadLengthEncodedString(b []byte) ([]byte, bool, int, error) {
	// Get length
	num, isNull, n := ReadLengthEncodedInteger(b)
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
