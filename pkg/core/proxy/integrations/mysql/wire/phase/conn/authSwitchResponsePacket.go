//go:build linux

package conn

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref:https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_switch_response.html

func DecodeAuthSwitchResponse(_ context.Context, data []byte) (*mysql.AuthSwitchResponsePacket, error) {
	return &mysql.AuthSwitchResponsePacket{
		Data: base64.StdEncoding.EncodeToString(data),
	}, nil
}

func EncodeAuthSwitchResponse(_ context.Context, packet *mysql.AuthSwitchResponsePacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write Data
	if _, err := buf.WriteString(packet.Data); err != nil {
		return nil, errors.New("failed to write Data for AuthSwitchResponse packet")
	}

	return buf.Bytes(), nil
}
