
package utility

import (
	"context"
	"encoding/binary"
	"fmt"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//COM_SET_OPTION: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_set_option.html

func DecodeSetOption(_ context.Context, data []byte) (*mysql.SetOptionPacket, error) {
	if len(data) < 3 {
		return nil, fmt.Errorf("set option packet too short")
	}

	packet := &mysql.SetOptionPacket{
		Status: data[0],
		Option: binary.LittleEndian.Uint16(data[1:3]),
	}

	return packet, nil
}
