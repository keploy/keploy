//go:build linux

// Package phase contains the encoding and decoding functions for the different phases of the MySQL protocol. And also contains the same for EOF, ERR, and OK packets.
package phase

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
)

// EOF packets (encoding & decoding)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_eof_packet.html

func DecodeEOF(_ context.Context, data []byte, capabilities uint32) (*mysql.EOFPacket, error) {
	if len(data) > 5 {
		return nil, fmt.Errorf("EOF packet too long for EOF")
	}

	packet := &mysql.EOFPacket{
		Header: data[0],
	}

	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		packet.Warnings = binary.LittleEndian.Uint16(data[1:3])
		packet.StatusFlags = binary.LittleEndian.Uint16(data[3:5])
	}

	return packet, nil
}

func EncodeEOF(_ context.Context, packet *mysql.EOFPacket, capabilities uint32) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write the header
	if err := buf.WriteByte(packet.Header); err != nil {
		return nil, fmt.Errorf("failed to write header: %w", err)
	}

	// Write the warnings and status flags if CLIENT_PROTOCOL_41 is set
	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		if err := binary.Write(buf, binary.LittleEndian, packet.Warnings); err != nil {
			return nil, fmt.Errorf("failed to write warnings for eof packet: %w", err)
		}

		if err := binary.Write(buf, binary.LittleEndian, packet.StatusFlags); err != nil {
			return nil, fmt.Errorf("failed to write status flags for eof packet: %w", err)
		}
	}

	return buf.Bytes(), nil
}

// ERR packets (encoding & decoding)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_err_packet.html

func DecodeERR(_ context.Context, data []byte, capabilities uint32) (*mysql.ERRPacket, error) {
	if len(data) < 9 {
		return nil, fmt.Errorf("ERR packet too short")
	}

	packet := &mysql.ERRPacket{
		Header: data[0],
	}

	pos := 1
	packet.ErrorCode = binary.LittleEndian.Uint16(data[pos : pos+2])
	pos += 2

	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		if data[pos] != '#' {
			return nil, fmt.Errorf("invalid SQL state marker: %c", data[pos])
		}
		packet.SQLStateMarker = string(data[pos])

		pos++
		packet.SQLState = string(data[pos : pos+5])
		pos += 5
	}

	packet.ErrorMessage = string(data[pos:])
	return packet, nil
}

func EncodeErr(_ context.Context, packet *mysql.ERRPacket, capabilities uint32) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write the header
	if err := buf.WriteByte(packet.Header); err != nil {
		return nil, fmt.Errorf("failed to write header: %w", err)
	}

	// Write the error code
	if err := binary.Write(buf, binary.LittleEndian, packet.ErrorCode); err != nil {
		return nil, fmt.Errorf("failed to write error code: %w", err)
	}

	// Write the SQL state marker and SQL state if CLIENT_PROTOCOL_41 is set
	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		if len(packet.SQLStateMarker) != 1 || len(packet.SQLState) != 5 {
			return nil, fmt.Errorf("invalid SQL state marker or SQL state length")
		}

		if err := buf.WriteByte(packet.SQLStateMarker[0]); err != nil {
			return nil, fmt.Errorf("failed to write SQL state marker: %w", err)
		}

		if _, err := buf.WriteString(packet.SQLState); err != nil {
			return nil, fmt.Errorf("failed to write SQL state: %w", err)
		}
	}

	// Write the error message
	if _, err := buf.WriteString(packet.ErrorMessage); err != nil {
		return nil, fmt.Errorf("failed to write error message: %w", err)
	}

	return buf.Bytes(), nil
}

// ref:https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_ok_packet.html

func DecodeOk(_ context.Context, data []byte, capabilities uint32) (*mysql.OKPacket, error) {
	if len(data) < 7 {
		return nil, fmt.Errorf("OK packet too short")
	}

	packet := &mysql.OKPacket{
		Header: data[0],
	}

	var n int
	var pos = 1

	packet.AffectedRows, _, n = utils.ReadLengthEncodedInteger(data[pos:])
	pos += n
	packet.LastInsertID, _, n = utils.ReadLengthEncodedInteger(data[pos:])
	pos += n

	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		packet.StatusFlags = binary.LittleEndian.Uint16(data[pos:])
		pos += 2
		packet.Warnings = binary.LittleEndian.Uint16(data[pos:])
		pos += 2
	} else if capabilities&uint32(mysql.CLIENT_TRANSACTIONS) > 0 {
		packet.StatusFlags = binary.LittleEndian.Uint16(data[pos:])
		pos += 2
	}

	// new ok package will check CLIENT_SESSION_TRACK too, but it is not supported in this version

	if pos < len(data) {
		packet.Info = string(data[pos:])
	}

	return packet, nil
}

func EncodeOk(_ context.Context, packet *mysql.OKPacket, capabilities uint32) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write Header
	if err := buf.WriteByte(packet.Header); err != nil {
		return nil, fmt.Errorf("failed to write Header: %w", err)
	}

	// Write Affected Rows
	if err := utils.WriteLengthEncodedInteger(buf, packet.AffectedRows); err != nil {
		return nil, fmt.Errorf("failed to write AffectedRows for OK packet: %w", err)
	}

	// Write Last Insert ID
	if err := utils.WriteLengthEncodedInteger(buf, packet.LastInsertID); err != nil {
		return nil, fmt.Errorf("failed to write LastInsertID for OK packet: %w", err)
	}

	// Write Status Flags and Warnings
	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		if err := binary.Write(buf, binary.LittleEndian, packet.StatusFlags); err != nil {
			return nil, fmt.Errorf("failed to write StatusFlags for OK packet: %w", err)
		}
		if err := binary.Write(buf, binary.LittleEndian, packet.Warnings); err != nil {
			return nil, fmt.Errorf("failed to write Warnings for OK packet: %w", err)
		}
	} else if capabilities&uint32(mysql.CLIENT_TRANSACTIONS) > 0 {
		if err := binary.Write(buf, binary.LittleEndian, packet.StatusFlags); err != nil {
			return nil, fmt.Errorf("failed to write StatusFlags for OK packet: %w", err)
		}
	}

	// Write Info
	if packet.Info != "" {
		if _, err := buf.WriteString(packet.Info); err != nil {
			return nil, fmt.Errorf("failed to write Info for OK packet: %w", err)
		}
	}

	return buf.Bytes(), nil
}
