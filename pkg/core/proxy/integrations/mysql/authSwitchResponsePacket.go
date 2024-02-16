package mysql

import (
	"go.keploy.io/server/pkg/models"
)

type AuthSwitchResponsePacket struct {
	AuthResponseData string `json:"auth_response_data,omitempty" yaml:"auth_response_data,omitempty"`
}

func decodeAuthSwitchResponse(data []byte) (*AuthSwitchResponsePacket, error) {
	return &AuthSwitchResponsePacket{
		AuthResponseData: string(data),
	}, nil
}
func encodeAuthSwitchResponse(packet *models.AuthSwitchResponsePacket) ([]byte, error) {
	return []byte(packet.AuthResponseData), nil
}
