//go:build linux

package connection

import (
	"bytes"
	"context"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_switch_request.html

func DecodeAuthSwitchRequest(_ context.Context, data []byte) (*mysql.AuthSwitchRequestPacket, error) {

	packet := &mysql.AuthSwitchRequestPacket{
		StatusTag: data[0],
	}

	// Splitting data by null byte to get plugin name and auth data
	parts := bytes.SplitN(data[1:], []byte{0x00}, 2)
	packet.PluginName = string(parts[0])
	if len(parts) > 1 {
		packet.PluginData = string(parts[1])
	}

	return packet, nil
}
