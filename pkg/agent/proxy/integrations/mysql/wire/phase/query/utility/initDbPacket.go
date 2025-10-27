// Package utility provides encoding and decoding of utility command packets.
package utility

import (
	"context"

	"github.com/keploy/keploy-integrations-shared/pkg/models/mysql"
)

//COM_INIT_DB: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_init_db.html

func DecodeInitDb(_ context.Context, data []byte) (*mysql.InitDBPacket, error) {
	packet := &mysql.InitDBPacket{
		Command: data[0],
		Schema:  string(data[1:]),
	}
	return packet, nil
}
