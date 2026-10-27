package conn

import (
	"bytes"
	"context"
	"fmt"

	"go.keploy.io/server/v3/pkg/models/mysql"
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

	// The symbolic caching_sha2_password mechanism a recording holds is
	// written as its status byte. Into a copy: packet is the recorded mock's,
	// shared by every connection replay serves from it, and rewriting it raced
	// with connections reading it, and lost the name they rank it by.
	data := packet.Data
	switch data {
	case mysql.CachingSha2PasswordToString(mysql.PerformFullAuthentication):
		data = string(mysql.PerformFullAuthentication)
	case mysql.CachingSha2PasswordToString(mysql.FastAuthSuccess):
		data = string(mysql.FastAuthSuccess)
	}

	// Write Data
	if _, err := buf.WriteString(data); err != nil {
		return nil, fmt.Errorf("failed to write Data for authMoreData packet: %w", err)
	}

	return buf.Bytes(), nil
}
