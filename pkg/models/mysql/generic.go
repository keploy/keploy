package mysql

// This file contains structs for mysql generic response packets
//refer: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_response_packets.html

// OKPacket represents the OK packet sent by the server to the client, it represents a successful completion of a command
type OKPacket struct {
	Header       byte   `json:"header" yaml:"header"`
	AffectedRows uint64 `json:"affected_rows,omitempty" yaml:"affected_rows"`
	LastInsertID uint64 `json:"last_insert_id,omitempty" yaml:"last_insert_id"`
	StatusFlags  uint16 `json:"status_flags,omitempty" yaml:"status_flags"`
	Warnings     uint16 `json:"warnings,omitempty" yaml:"warnings"`
	Info         string `json:"info,omitempty" yaml:"info"`
}

// ERRPacket represents the ERR packet sent by the server to the client, it represents an error occurred during the execution of a command
type ERRPacket struct {
	Header         byte   `yaml:"header"`
	ErrorCode      uint16 `yaml:"error_code"`
	SQLStateMarker string `yaml:"sql_state_marker"`
	SQLState       string `yaml:"sql_state"`
	ErrorMessage   string `yaml:"error_message"`
}

// EOFPacket represents the EOF packet sent by the server to the client, it represents the end of a query execution result
type EOFPacket struct {
	Header      byte   `yaml:"header"`
	Warnings    uint16 `yaml:"warnings"`
	StatusFlags uint16 `yaml:"status_flags"`
}
