package mysql

import (
	"encoding/gob"
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

func init() {
	// Register all struct types from mysql.go
	gob.Register(&Spec{})
	gob.Register(&RequestYaml{})
	gob.Register(&ResponseYaml{})
	gob.Register(&PacketInfo{})
	gob.Register(&Request{})
	gob.Register(&Response{})
	gob.Register(&PacketBundle{})
	gob.Register(&Packet{})
	gob.Register(&Header{})

	// Register all struct types from comm.go
	gob.Register(&QueryPacket{})
	gob.Register(&LocalInFileRequestPacket{})
	gob.Register(&TextResultSet{})
	gob.Register(&BinaryProtocolResultSet{})
	gob.Register(&GenericResponse{})
	gob.Register(&ColumnCount{})
	gob.Register(&ColumnDefinition41{})
	gob.Register(&TextRow{})
	gob.Register(&BinaryRow{})
	gob.Register(&ColumnEntry{})
	gob.Register(&StmtPreparePacket{})
	gob.Register(&StmtPrepareOkPacket{})
	gob.Register(&StmtExecutePacket{})
	gob.Register(&Parameter{})
	gob.Register(&StmtFetchPacket{})
	gob.Register(&StmtClosePacket{})
	gob.Register(&StmtResetPacket{})
	gob.Register(&StmtSendLongDataPacket{})
	gob.Register(&QuitPacket{})
	gob.Register(&InitDBPacket{})
	gob.Register(&StatisticsPacket{})
	gob.Register(&DebugPacket{})
	gob.Register(&PingPacket{})
	gob.Register(&ResetConnectionPacket{})
	gob.Register(&SetOptionPacket{})
	gob.Register(&ChangeUserPacket{})

	// Register all struct types from generic.go
	gob.Register(&OKPacket{})
	gob.Register(&ERRPacket{})
	gob.Register(&EOFPacket{})

	// Register all struct types from conn.go
	gob.Register(&HandshakeV10Packet{})
	gob.Register(&HandshakeResponse41Packet{})
	gob.Register(&SSLRequestPacket{})
	gob.Register(&AuthSwitchRequestPacket{})
	gob.Register(&AuthSwitchResponsePacket{})
	gob.Register(&AuthMoreDataPacket{})
	gob.Register(&AuthNextFactorPacket{})
}
