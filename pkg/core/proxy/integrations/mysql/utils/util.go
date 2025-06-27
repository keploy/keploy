//go:build linux

// Package utils provides utility functions for MySQL packets
package utils

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models/mysql"
	"go.keploy.io/server/v2/utils"
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

// ReadPacketStream reads packets from the connection and sends them to the bufferChannel
func ReadPacketStream(ctx context.Context, logger *zap.Logger, conn net.Conn, bufferChannel chan []byte, errChannel chan error) {
	for {
		select {
		case <-ctx.Done():
			// errChannel <- ctx.Err()
			return
		default:
			if conn == nil {
				logger.Debug("the conn is nil")
			}
			buffer, err := ReadPacketBuffer(ctx, logger, conn)
			if err != nil {
				if ctx.Err() != nil { // to avoid sending buffer to closed channel if the context is cancelled
					return
				}
				if err != io.EOF {
					utils.LogError(logger, err, "failed to read mysql packet buffer")
				}
				errChannel <- err
				return
			}
			if ctx.Err() != nil { // to avoid sending buffer to closed channel if the context is cancelled
				return
			}
			bufferChannel <- buffer
		}
	}
}

// ReadPacketBuffer reads a MySQL packet from the connection
func ReadPacketBuffer(ctx context.Context, logger *zap.Logger, conn net.Conn) ([]byte, error) {
	var packetBuffer []byte
	// first read the header length
	header, err := util.ReadRequiredBytes(ctx, logger, conn, 4)
	if err != nil {
		if err == io.EOF {
			return nil, err
		}
		// return packetBuffer, fmt.Errorf("failed to read mysql packet header: %w", err)
		return packetBuffer, err
	}

	packetBuffer = append(packetBuffer, header...)

	// read the payload length
	payloadLength := GetPayloadLength(header[:3])
	if payloadLength > 0 {
		payload, err := util.ReadRequiredBytes(ctx, logger, conn, int(payloadLength))
		if err != nil {
			if err == io.EOF {
				return nil, err
			}
			// return packetBuffer, fmt.Errorf("failed to read mysql packet payload: %w", err)
			return packetBuffer, err
		}
		packetBuffer = append(packetBuffer, payload...)
	}

	return packetBuffer, nil
}

// BytesToMySQLPacket converts a byte slice to a MySQL packet
func BytesToMySQLPacket(buffer []byte) (mysql.Packet, error) {
	if len(buffer) < 4 {
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
func GetPayloadLength(src []byte) (length uint32) {
	length = uint32(src[0]) | uint32(src[1])<<8 | uint32(src[2])<<16
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
	if len(data) < 5 {
		return false // Packet is too short to be valid
	}

	// Check if the first byte is 5 or 7
	if data[0] != 5 && data[0] != 7 {
		return false
	}

	// Check if the packet contains the EOF marker 0xFE
	return len(data) > 0 && data[4] == 0xFE
}

func IsERRPacket(data []byte) bool {
	return len(data) > 9 && data[4] == mysql.ERR
}

func IsOKPacket(data []byte) bool {
	return len(data) > 7 && data[4] == mysql.OK
}

func IsGenericResponse(data []byte) (string, bool) {
	if IsOKPacket(data) {
		return "OK", true
	}
	if IsERRPacket(data) {
		return "ERR", true
	}
	if IsEOFPacket(data) {
		return "EOF", true
	}
	return "", false
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

// ReadNullTerminatedString reads a null-terminated string from a byte slice
func ReadNullTerminatedString(b []byte) ([]byte, int, error) {
	i := bytes.IndexByte(b, 0x00)
	if i == -1 {
		return nil, 0, io.EOF
	}
	return b[:i], i + 1, nil
}

func WriteStream(ctx context.Context, logger *zap.Logger, conn net.Conn, buff [][]byte) error {
	for _, b := range buff {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := conn.Write(b)
		if err != nil {
			utils.LogError(logger, err, "failed to write to connection")
			return err
		}
	}
	return nil
}

func WriteLengthEncodedString(buf *bytes.Buffer, s string) error {
	length := len(s)
	if err := WriteLengthEncodedInteger(buf, uint64(length)); err != nil {
		return err
	}
	if _, err := buf.WriteString(s); err != nil {
		return err
	}
	return nil
}

func WriteLengthEncodedInteger(buf *bytes.Buffer, num uint64) error {
	switch {
	case num <= 250:
		if err := buf.WriteByte(byte(num)); err != nil {
			return err
		}
	case num <= 0xFFFF:
		if err := buf.WriteByte(0xFC); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.LittleEndian, uint16(num)); err != nil {
			return err
		}
	case num <= 0xFFFFFF:
		if err := buf.WriteByte(0xFD); err != nil {
			return err
		}
		num24 := []byte{byte(num), byte(num >> 8), byte(num >> 16)}
		if _, err := buf.Write(num24); err != nil {
			return err
		}
	default:
		if err := buf.WriteByte(0xFE); err != nil {
			return err
		}
		if err := binary.Write(buf, binary.LittleEndian, num); err != nil {
			return err
		}
	}
	return nil
}

func WriteUint24(buf *bytes.Buffer, value uint32) error {
	if value > 0xFFFFFF {
		return errors.New("value exceeds 24 bits")
	}
	buf.WriteByte(byte(value))
	buf.WriteByte(byte(value >> 8))
	buf.WriteByte(byte(value >> 16))
	return nil
}

func ParseBinaryDate(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	year := binary.LittleEndian.Uint16(b[1:3])
	month := b[3]
	day := b[4]
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day), int(length) + 1, nil
}

func ParseBinaryDateTime(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	year := binary.LittleEndian.Uint16(b[1:3])
	month := b[3]
	day := b[4]
	hour := b[5]
	minute := b[6]
	second := b[7]
	if length > 7 {
		microsecond := binary.LittleEndian.Uint32(b[8:12])
		return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d.%06d", year, month, day, hour, minute, second, microsecond), int(length) + 1, nil
	}
	return fmt.Sprintf("%04d-%02d-%02d %02d:%02d:%02d", year, month, day, hour, minute, second), int(length) + 1, nil
}

func ParseBinaryTime(b []byte) (interface{}, int, error) {
	if len(b) == 0 {
		return nil, 0, nil
	}
	length := b[0]
	if length == 0 {
		return nil, 1, nil
	}
	isNegative := b[1] == 1
	days := binary.LittleEndian.Uint32(b[2:6])
	hours := b[6]
	minutes := b[7]
	seconds := b[8]
	var microseconds uint32
	if length > 8 {
		microseconds = binary.LittleEndian.Uint32(b[9:13])
	}
	timeString := fmt.Sprintf("%d %02d:%02d:%02d.%06d", days, hours, minutes, seconds, microseconds)
	if isNegative {
		timeString = "-" + timeString
	}
	return timeString, int(length) + 1, nil
}
