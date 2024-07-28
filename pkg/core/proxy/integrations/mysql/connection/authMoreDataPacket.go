//go:build linux

package connection

import (
	"context"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_more_data.html

func DecodeAuthMoreData(_ context.Context, data []byte) (*mysql.AuthMoreDataPacket, error) {
	return &mysql.AuthMoreDataPacket{
		StatusTag: data[0],
		Data:      string(data[1:]),
	}, nil
}
