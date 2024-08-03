//go:build linux

package connection

import (
	"bytes"
	"errors"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_switch_request.html

func DecodeAuthSwitchRequest(data []byte) (*mysql.AuthSwitchRequestPacket, error) {

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
func EncodeAuthSwitchRequest(packet *mysql.AuthSwitchRequestPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write StatusTag
	if err := buf.WriteByte(packet.StatusTag); err != nil {
		return nil, errors.New("failed to write StatusTag")
	}

	// Write PluginName followed by a null terminator
	if _, err := buf.WriteString(packet.PluginName); err != nil {
		return nil, errors.New("failed to write PluginName")
	}
	if err := buf.WriteByte(0x00); err != nil {
		return nil, errors.New("failed to write null terminator for PluginName")
	}

	// Write PluginData
	if _, err := buf.WriteString(packet.PluginData); err != nil {
		return nil, errors.New("failed to write PluginData")
	}

	return buf.Bytes(), nil
}

// Define the AuthSwitchRequestPacket struct
type AuthSwitchRequestPacket struct {
	StatusTag  byte   `yaml:"status_tag"`
	PluginName string `yaml:"plugin_name"`
	PluginData string `yaml:"plugin_data"`
}
