//go:build linux

// Package mysql provides integration with MySQL outgoing.
package mysql

import (
	"errors"

	"go.keploy.io/server/v2/pkg/models"
)

func decodeAuthMoreData(data []byte) (*models.NextAuthPacket, error) {
	if data[0] != 0x02 {
		return nil, errors.New("invalid packet type for NextAuthPacket")
	}
	return &models.NextAuthPacket{
		PluginData: data[0],
	}, nil
}

// Encode function for Next Authentication method Packet
//func encodeAuthMoreData(packet *NextAuthPacket) ([]byte, error) {
//	if packet.PluginData != 0x02 {
//		return nil, errors.New("invalid PluginData value for NextAuthPacket")
//	}
//	return []byte{packet.PluginData}, nil
//}
