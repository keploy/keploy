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
	ReqTimestampMock time.Time         `json:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time         `json:"resTimestampMock,omitempty"`
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
	// gob.RegisterName with an EXPLICIT wire name, not gob.Register, so the name
	// gob writes into every mocks.gob (and the interface payloads streamed
	// between the keploy binaries) is fixed here rather than derived from the Go
	// package path and type name. gob.Register(&T{}) keys the wire on the live
	// identifier, so renaming or moving one of these types silently changes the
	// on-disk format and makes previously recorded mocks fail to decode — the
	// stability gap env-vars.md flags for the gob format. Each literal below is
	// exactly the name gob.Register produced before, so this is byte-identical on
	// the wire (verified by the round-trip + golden tests in gob_register_test.go)
	// and existing mocks keep decoding; the difference is only that a future
	// rename can no longer move it. Frozen: change a literal only with intent, and
	// update the golden test.
	gob.RegisterName("*mysql.Spec", &Spec{})
	gob.RegisterName("*mysql.RequestYaml", &RequestYaml{})
	gob.RegisterName("*mysql.ResponseYaml", &ResponseYaml{})
	gob.RegisterName("*mysql.PacketInfo", &PacketInfo{})
	gob.RegisterName("*mysql.Request", &Request{})
	gob.RegisterName("*mysql.Response", &Response{})
	gob.RegisterName("*mysql.PacketBundle", &PacketBundle{})
	gob.RegisterName("*mysql.Packet", &Packet{})
	gob.RegisterName("*mysql.Header", &Header{})

	// Register all struct types from comm.go
	gob.RegisterName("*mysql.QueryPacket", &QueryPacket{})
	gob.RegisterName("*mysql.LocalInFileRequestPacket", &LocalInFileRequestPacket{})
	gob.RegisterName("*mysql.TextResultSet", &TextResultSet{})
	gob.RegisterName("*mysql.BinaryProtocolResultSet", &BinaryProtocolResultSet{})
	gob.RegisterName("*mysql.GenericResponse", &GenericResponse{})
	gob.RegisterName("*mysql.ColumnCount", &ColumnCount{})
	gob.RegisterName("*mysql.ColumnDefinition41", &ColumnDefinition41{})
	gob.RegisterName("*mysql.TextRow", &TextRow{})
	gob.RegisterName("*mysql.BinaryRow", &BinaryRow{})
	gob.RegisterName("*mysql.ColumnEntry", &ColumnEntry{})
	gob.RegisterName("*mysql.StmtPreparePacket", &StmtPreparePacket{})
	gob.RegisterName("*mysql.StmtPrepareOkPacket", &StmtPrepareOkPacket{})
	gob.RegisterName("*mysql.StmtExecutePacket", &StmtExecutePacket{})
	gob.RegisterName("*mysql.Parameter", &Parameter{})
	gob.RegisterName("*mysql.StmtFetchPacket", &StmtFetchPacket{})
	gob.RegisterName("*mysql.StmtClosePacket", &StmtClosePacket{})
	gob.RegisterName("*mysql.StmtResetPacket", &StmtResetPacket{})
	gob.RegisterName("*mysql.StmtSendLongDataPacket", &StmtSendLongDataPacket{})
	gob.RegisterName("*mysql.QuitPacket", &QuitPacket{})
	gob.RegisterName("*mysql.InitDBPacket", &InitDBPacket{})
	gob.RegisterName("*mysql.StatisticsPacket", &StatisticsPacket{})
	gob.RegisterName("*mysql.DebugPacket", &DebugPacket{})
	gob.RegisterName("*mysql.PingPacket", &PingPacket{})
	gob.RegisterName("*mysql.ResetConnectionPacket", &ResetConnectionPacket{})
	gob.RegisterName("*mysql.SetOptionPacket", &SetOptionPacket{})
	gob.RegisterName("*mysql.ChangeUserPacket", &ChangeUserPacket{})

	// Register all struct types from generic.go
	gob.RegisterName("*mysql.OKPacket", &OKPacket{})
	gob.RegisterName("*mysql.ERRPacket", &ERRPacket{})
	gob.RegisterName("*mysql.EOFPacket", &EOFPacket{})

	// Register all struct types from conn.go
	gob.RegisterName("*mysql.HandshakeV10Packet", &HandshakeV10Packet{})
	gob.RegisterName("*mysql.HandshakeResponse41Packet", &HandshakeResponse41Packet{})
	gob.RegisterName("*mysql.SSLRequestPacket", &SSLRequestPacket{})
	gob.RegisterName("*mysql.AuthSwitchRequestPacket", &AuthSwitchRequestPacket{})
	gob.RegisterName("*mysql.AuthSwitchResponsePacket", &AuthSwitchResponsePacket{})
	gob.RegisterName("*mysql.AuthMoreDataPacket", &AuthMoreDataPacket{})
	gob.RegisterName("*mysql.AuthNextFactorPacket", &AuthNextFactorPacket{})
}
