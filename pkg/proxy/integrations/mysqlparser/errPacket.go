package mysqlparser

import (
	"encoding/binary"
	"fmt"
)

type ERRPacket struct {
	Header         byte   `yaml:"header"`
	ErrorCode      uint16 `yaml:"error_code"`
	SQLStateMarker string `yaml:"sql_state_marker"`
	SQLState       string `yaml:"sql_state"`
	ErrorMessage   string `yaml:"error_message"`
}

func decodeMySQLErr(data []byte) (*ERRPacket, error) {
	if len(data) < 9 {
		return nil, fmt.Errorf("ERR packet too short")
	}
	if data[0] != 0xff {
		return nil, fmt.Errorf("invalid ERR packet header: %x", data[0])
	}

	packet := &ERRPacket{}
	packet.ErrorCode = binary.LittleEndian.Uint16(data[1:3])

	if data[3] != '#' {
		return nil, fmt.Errorf("invalid SQL state marker: %c", data[3])
	}
	packet.SQLState = string(data[4:9])
	packet.ErrorMessage = string(data[9:])
	return packet, nil
}
