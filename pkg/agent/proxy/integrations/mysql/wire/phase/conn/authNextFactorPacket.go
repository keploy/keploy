//go:build linux

package conn

import (
	"bytes"
	"context"
	"errors"
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

func EncodeAuthNextFactor(_ context.Context, packet *mysql.AuthNextFactorPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write PacketType
	if err := buf.WriteByte(packet.PacketType); err != nil {
		return nil, errors.New("failed to write PacketType")
	}

	// Write PluginName followed by a null terminator
	if _, err := buf.WriteString(packet.PluginName); err != nil {
		return nil, errors.New("failed to write PluginName for AuthNextFactor packet")
	}

	if err := buf.WriteByte(0x00); err != nil {
		return nil, errors.New("failed to write null terminator for PluginName for AuthNextFactor packet")
	}

	// Write PluginData
	if _, err := buf.WriteString(packet.PluginData); err != nil {
		return nil, errors.New("failed to write PluginData for AuthNextFactor packet")
	}

	return buf.Bytes(), nil
}
