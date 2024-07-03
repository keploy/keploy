//go:build linux

package mysql

import (
	"go.keploy.io/server/v2/pkg/models"
)

func decodeAuthSwitchResponse(data []byte) (*models.AuthSwitchResponsePacket, error) {
	return &models.AuthSwitchResponsePacket{
		AuthResponseData: string(data),
	}, nil
}
func encodeAuthSwitchResponse(packet *models.AuthSwitchResponsePacket) ([]byte, error) {
	return []byte(packet.AuthResponseData), nil
}
