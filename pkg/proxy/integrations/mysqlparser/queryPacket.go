package mysqlparser

import "fmt"

type QueryPacket struct {
	Command byte   `json:"command,omitempty" yaml:"command,omitempty,flow"`
	Query   string `json:"query,omitempty" yaml:"query,omitempty,flow"`
}

func decodeMySQLQuery(data []byte) (*QueryPacket, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("query packet too short")
	}

	packet := &QueryPacket{}
	packet.Command = data[0]
	packet.Query = string(data[1:])

	return packet, nil
}
