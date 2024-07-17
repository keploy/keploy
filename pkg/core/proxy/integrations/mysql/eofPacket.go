//go:build linux

package mysql

import (
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeMYSQLEOF(data []byte, serverGreetings *models.MySQLHandshakeV10Packet) (*models.EOFPacket, error) {
	if len(data) > 5 {
		return nil, fmt.Errorf("EOF packet too long for EOF")
	}

	packet := &models.EOFPacket{}
	packet.Header = data[0]

	if serverGreetings.CapabilityFlags&uint32(models.CLIENT_PROTOCOL_41) > 0 {
		packet.Warnings = binary.LittleEndian.Uint16(data[1:3])
		packet.StatusFlags = binary.LittleEndian.Uint16(data[3:5])
	}

	return packet, nil
}
