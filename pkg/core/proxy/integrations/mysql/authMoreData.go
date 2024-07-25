//go:build linux

// Package mysql provides integration with MySQL outgoing.
package mysql

import (
	"errors"

	"go.keploy.io/server/v2/pkg/models"
)

// DecodeAuthMoreData decodes the Auth more data packet.
func DecodeAuthMoreData(data []byte) (*models.AuthMoreDataPacket, error) {
	if data[0] != 0x01 {
		return nil, errors.New("invalid packet type for Auth more data")
	}
	return &models.AuthMoreDataPacket{
		Data: data[1:],
	}, nil
}
