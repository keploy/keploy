// Package mysql provides integration with MySQL outgoing.
package mysql

import (
	"errors"
)

type NextAuthPacket struct {
	PluginData byte `yaml:"plugin_data"`
}

func decodeAuthMoreData(data []byte) (*NextAuthPacket, error) {
	if data[0] != 0x02 {
		return nil, errors.New("invalid packet type for NextAuthPacket")
	}
	return &NextAuthPacket{
		PluginData: data[0],
	}, nil
}
