package mysqlparser

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

// Encode function for Next Authentication method Packet
func encodeAuthMoreData(packet *NextAuthPacket) ([]byte, error) {
	if packet.PluginData != 0x02 {
		return nil, errors.New("invalid PluginData value for NextAuthPacket")
	}
	return []byte{packet.PluginData}, nil
}
