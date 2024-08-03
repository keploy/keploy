//go:build linux

// Package generic provides encoding and decoding of generic response packets.
package generic

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

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

func EncodeEOF(ctx context.Context, packet *mysql.EOFPacket, capabilities uint32) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write the header
	if err := buf.WriteByte(packet.Header); err != nil {
		return nil, fmt.Errorf("failed to write header: %w", err)
	}

	// Write the warnings and status flags if CLIENT_PROTOCOL_41 is set
	if capabilities&uint32(mysql.CLIENT_PROTOCOL_41) > 0 {
		if err := binary.Write(buf, binary.LittleEndian, packet.Warnings); err != nil {
			return nil, fmt.Errorf("failed to write warnings: %w", err)
		}

		if err := binary.Write(buf, binary.LittleEndian, packet.StatusFlags); err != nil {
			return nil, fmt.Errorf("failed to write status flags: %w", err)
		}
	}

	return buf.Bytes(), nil
}
