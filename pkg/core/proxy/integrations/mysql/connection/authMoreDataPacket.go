//go:build linux

package connection

import (
	"bytes"
	"context"
	"fmt"

	"go.keploy.io/server/v2/pkg/models/mysql"
)

//ref: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_more_data.html

func DecodeAuthMoreData(_ context.Context, data []byte) (*mysql.AuthMoreDataPacket, error) {
	return &mysql.AuthMoreDataPacket{
		StatusTag: data[0],
		Data:      string(data[1:]),
	}, nil
}

func EncodeAuthMoreData(_ context.Context, packet *mysql.AuthMoreDataPacket) ([]byte, error) {
	buf := new(bytes.Buffer)

	// Write StatusTag
	if err := buf.WriteByte(packet.StatusTag); err != nil {
		return nil, fmt.Errorf("failed to write StatusTag for AuthMoreData packet: %w", err)
	}

	// Write Data
	if _, err := buf.WriteString(packet.Data); err != nil {
		return nil, fmt.Errorf("failed to write Data for authMoreData packet: %w", err)
	}

	return buf.Bytes(), nil
}
