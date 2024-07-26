//go:build linux

package generic

import (
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
)

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
