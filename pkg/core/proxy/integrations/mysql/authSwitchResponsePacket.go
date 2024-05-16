package mysql

import (
	"go.keploy.io/server/v2/pkg/models"
)

type AuthSwitchResponsePacket struct {
	AuthResponseData string `yaml:"auth_response_data"`
}

func decodeAuthSwitchResponse(data []byte) (*AuthSwitchResponsePacket, error) {
	return &AuthSwitchResponsePacket{
		AuthResponseData: string(data),
	}, nil
}
func encodeAuthSwitchResponse(packet *models.AuthSwitchResponsePacket) ([]byte, error) {
	return []byte(packet.AuthResponseData), nil
}
