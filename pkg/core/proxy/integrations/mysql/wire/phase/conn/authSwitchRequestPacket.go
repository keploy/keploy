//go:build linux

package conn

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"

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
		packet.PluginData = base64.RawStdEncoding.EncodeToString(parts[1])
	}

	return packet, nil
}

func EncodeAuthSwitchRequest(_ context.Context, packet *mysql.AuthSwitchRequestPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write StatusTag
	if err := buf.WriteByte(packet.StatusTag); err != nil {
		return nil, errors.New("failed to write StatusTag")
	}

	// Write PluginName followed by a null terminator
	if _, err := buf.WriteString(packet.PluginName); err != nil {
		return nil, errors.New("failed to write PluginName for AuthSwitchRequest packet")
	}
	if err := buf.WriteByte(0x00); err != nil {
		return nil, errors.New("failed to write null terminator for PluginName for AuthSwitchRequest packet")
	}

	pluginData, err := base64.RawStdEncoding.DecodeString(packet.PluginData)
	if err != nil {
		return nil, errors.New("failed to decode PluginData for AuthSwitchRequest packet")
	}

	// Write PluginData
	if _, err := buf.WriteString(string(pluginData)); err != nil {
		return nil, errors.New("failed to write PluginData for AuthSwitchRequest packet")
	}

	return buf.Bytes(), nil
}
