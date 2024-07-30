//go:build linux

package connection

import (
	"context"
	"fmt"

	"go.keploy.io/server/v2/pkg/core/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_next_factor_request.html

func DecodeAuthNextFactor(_ context.Context, data []byte) (*mysql.AuthNextFactorPacket, error) {

	packet := &mysql.AuthNextFactorPacket{
		PacketType: data[0],
	}

	data, idx, err := utils.ReadNullTerminatedString(data[1:])
	if err != nil {
		return nil, fmt.Errorf("malformed handshake response packet: missing null terminator for PluginName")
	}
	packet.PluginName = string(data)
	packet.PluginData = string(data[idx:])

	return packet, nil
}
