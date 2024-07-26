//go:build linux

package generic

import (
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_err_packet.html

func DecodeErr(_ context.Context, data []byte, capabilities uint32) (*mysql.ERRPacket, error) {
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
