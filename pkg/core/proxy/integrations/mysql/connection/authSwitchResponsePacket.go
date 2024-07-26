//go:build linux

package connection

import (
	"context"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref:https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_switch_response.html

func DecodeAuthSwitchResponse(_ context.Context, data []byte) (*mysql.AuthSwitchResponsePacket, error) {
	return &mysql.AuthSwitchResponsePacket{
		Data: string(data),
	}, nil
}
