package mysqlparser

import (
	"encoding/binary"
	"fmt"
)

type ERRPacket struct {
	Header         byte   `json:"header,omitempty" yaml:"header,omitempty,flow"`
	ErrorCode      uint16 `json:"error_code,omitempty" yaml:"error_code,omitempty,flow"`
	SQLStateMarker string `json:"sql_state_marker,omitempty" yaml:"sql_state_marker,omitempty,flow"`
	SQLState       string `json:"sql_state,omitempty" yaml:"sql_state,omitempty,flow"`
	ErrorMessage   string `json:"error_message,omitempty" yaml:"error_message,omitempty,flow"`
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
