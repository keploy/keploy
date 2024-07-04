//go:build linux

package mysql

import (
	"fmt"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeMySQLQuery(data []byte) (*models.MySQLQueryPacket, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("query packet too short")
	}

	packet := &models.MySQLQueryPacket{}
	packet.Command = data[0]
	packet.Query = string(data[1:])

	return packet, nil
}
