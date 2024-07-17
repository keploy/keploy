//go:build linux

package mysql

import (
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeMySQLErr(data []byte, serverGreeting *models.MySQLHandshakeV10Packet) (*models.MySQLERRPacket, error) {
	if len(data) < 9 {
		return nil, fmt.Errorf("ERR packet too short")
	}

	packet := &models.MySQLERRPacket{}

	pos := 1
	packet.ErrorCode = binary.LittleEndian.Uint16(data[pos : pos+2])
	pos += 2

	if serverGreeting.CapabilityFlags&uint32(models.CLIENT_PROTOCOL_41) > 0 {
		if data[pos] != '#' {
			return nil, fmt.Errorf("invalid SQL state marker: %c", data[3])
		}
		pos++
		packet.SQLState = string(data[pos : pos+5])
		pos += 5
	}

	packet.ErrorMessage = string(data[pos:])
	return packet, nil
}
