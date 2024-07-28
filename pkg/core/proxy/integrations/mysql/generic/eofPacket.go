//go:build linux

// Package generic provides encoding and decoding of generic response packets.
package generic

import (
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
