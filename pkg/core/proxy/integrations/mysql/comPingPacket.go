//go:build linux

package mysql

import (
	"errors"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeComPing(data []byte) (models.ComPingPacket, error) {
	if len(data) < 1 || data[0] != 0x0e {
		return models.ComPingPacket{}, errors.New("Data malformed for COM_PING")
	}

	return models.ComPingPacket{}, nil
}
