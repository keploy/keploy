package mysqlparser

import (
	"bytes"
	"fmt"

	"go.keploy.io/server/pkg/models"
)

type AuthSwitchRequestPacket struct {
	StatusTag      byte   `yaml:"status_tag"`
	PluginName     string `yaml:"plugin_name"`
	PluginAuthData string `yaml:"plugin_authdata"`
}

func decodeAuthSwitchRequest(data []byte) (*AuthSwitchRequestPacket, error) {
	if len(data) < 1 || data[0] != 0xFE {
		return nil, fmt.Errorf("invalid AuthSwitchRequest packet")
	}

	packet := &AuthSwitchRequestPacket{
		StatusTag: data[0],
	}

	// Splitting data by null byte to get plugin name and auth data
	parts := bytes.SplitN(data[1:], []byte{0x00}, 2)
	packet.PluginName = string(parts[0])
	if len(parts) > 1 {
		packet.PluginAuthData = string(parts[1])
	}

	return packet, nil
}
func encodeAuthSwitchRequest(packet *models.AuthSwitchRequestPacket) ([]byte, error) {
	if packet.StatusTag != 0xFE {
		return nil, fmt.Errorf("invalid AuthSwitchRequest packet")
	}

	buf := new(bytes.Buffer)

	// Write the status tag
	buf.WriteByte(packet.StatusTag)

	// Write the plugin name
	buf.WriteString(packet.PluginName)
	buf.WriteByte(0x00) // Null byte separator

	// Write the plugin auth data
	buf.WriteString(packet.PluginAuthData)

	return buf.Bytes(), nil
}
