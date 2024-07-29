package mysql

import (
	"time"

	"gopkg.in/yaml.v3"
)

type Spec struct {
	Metadata         map[string]string `json:"metadata" yaml:"metadata"`
	Requests         []RequestYaml     `json:"requests" yaml:"requests"`
	Response         []ResponseYaml    `json:"responses" yaml:"responses"`
	CreatedAt        int64             `json:"created" yaml:"created,omitempty"`
	ReqTimestampMock time.Time         `json:"ReqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"ResTimestampMock,omitempty"`
}

type RequestYaml struct {
	Header  *PacketInfo       `json:"header,omitempty" yaml:"header"`
	Meta    map[string]string `json:"meta,omitempty" yaml:"meta,omitempty"`
	Message yaml.Node         `json:"message,omitempty" yaml:"message"`
}

type ResponseYaml struct {
	Header  *PacketInfo       `json:"header,omitempty" yaml:"header"`
	Meta    map[string]string `json:"meta,omitempty" yaml:"meta,omitempty"`
	Message yaml.Node         `json:"message,omitempty" yaml:"message"`
	Payload string            `json:"payload,omitempty" yaml:"payload,omitempty"`
}

type PacketInfo struct {
	Header *Header `json:"header" yaml:"header"`
	Type   string  `json:"packet_type" yaml:"packet_type"`
}

type Request struct {
	PacketBundle `json:"packet_bundle" yaml:"packet_bundle"`
}

type Response struct {
	PacketBundle `json:"packet_bundle" yaml:"packet_bundle"`
	Payload      string `json:"payload,omitempty" yaml:"payload,omitempty"`
}

type PacketBundle struct {
	Header  *PacketInfo       `json:"header" yaml:"header"`
	Message interface{}       `json:"message" yaml:"message"`
	Meta    map[string]string `json:"meta,omitempty" yaml:"meta,omitempty"`
}

// MySql Packet
//refer: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_packets.html

type Packet struct {
	Header  Header `json:"header" yaml:"header"`
	Payload []byte `json:"payload,omitempty" yaml:"payload,omitempty"`
}

type Header struct {
	PayloadLength uint32 `json:"payload_length" yaml:"payload_length"`
	SequenceID    uint8  `json:"sequence_id" yaml:"sequence_id"`
}
