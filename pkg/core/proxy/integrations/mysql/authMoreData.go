// Package mysql provides integration with MySQL outgoing.
package mysql

import (
	"errors"
)

type NextAuthPacket struct {
	PluginData byte `json:"plugin_data,omitempty" yaml:"plugin_data,omitempty"`
}

func decodeAuthMoreData(data []byte) (*NextAuthPacket, error) {
	if data[0] != 0x02 {
		return nil, errors.New("invalid packet type for NextAuthPacket")
	}
	return &NextAuthPacket{
		PluginData: data[0],
	}, nil
}
