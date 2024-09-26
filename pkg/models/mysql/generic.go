package mysql

// This file contains structs for mysql generic response packets
// refer: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_response_packets.html

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
	Header         byte   `json:"header" yaml:"header"`
	ErrorCode      uint16 `json:"error_code" yaml:"error_code"`
	SQLStateMarker string `json:"sql_state_marker" yaml:"sql_state_marker"`
	SQLState       string `json:"sql_state" yaml:"sql_state"`
	ErrorMessage   string `json:"error_message" yaml:"error_message"`
}

// EOFPacket represents the EOF packet sent by the server to the client, it represents the end of a query execution result
type EOFPacket struct {
	Header      byte   `json:"header" yaml:"header"`
	Warnings    uint16 `json:"warnings" yaml:"warnings"`
	StatusFlags uint16 `json:"status_flags" yaml:"status_flags"`
}
