package models

type MySQLPacketHeader struct {
	PacketLength uint32 `json:"packet_length,omitempty" yaml:"packet_length,omitempty,flow"`
	PacketNumber uint8  `json:"packet_number,omitempty" yaml:"packet_number,omitempty,flow"`
	PacketType   string `json:"packet_type,omitempty" yaml:"packet_type,omitempty,flow"`
}

type MySQLRequest struct {
	Header    *MySQLPacketHeader `json:"header,omitempty" yaml:"header,omitempty,flow"`
	Message   interface{}        `json:"message,omitempty" yaml:"message,omitempty,flow"`
	ReadDelay int64              `json:"read_delay,omitempty" yaml:"read_delay,omitempty,flow"`
}

type RowColumnDefinition struct {
	Type  FieldType   `json:"type,omitempty" yaml:"type,omitempty,flow"`
	Name  string      `json:"name,omitempty" yaml:"name,omitempty,flow"`
	Value interface{} `json:"value,omitempty" yaml:"value,omitempty,flow"`
}

type MySQLResponse struct {
	Header    *MySQLPacketHeader `json:"header,omitempty" yaml:"header,omitempty,flow"`
	Message   interface{}        `json:"message,omitempty" yaml:"message,omitempty,flow"`
	ReadDelay int64              `json:"read_delay,omitempty" yaml:"read_delay,omitempty,flow"`
}

type MySQLHandshakeV10Packet struct {
	ProtocolVersion uint8  `json:"protocol_version,omitempty" yaml:"protocol_version,omitempty,flow"`
	ServerVersion   string `json:"server_version,omitempty" yaml:"server_version,omitempty,flow"`
	ConnectionID    uint32 `json:"connection_id,omitempty" yaml:"connection_id,omitempty,flow"`
	AuthPluginData  string `json:"auth_plugin_data,omitempty" yaml:"auth_plugin_data,omitempty,flow"`
	CapabilityFlags uint32 `json:"capability_flags,omitempty" yaml:"capability_flags,omitempty,flow"`
	CharacterSet    uint8  `json:"character_set,omitempty" yaml:"character_set,omitempty,flow"`
	StatusFlags     uint16 `json:"status_flags,omitempty" yaml:"status_flags,omitempty,flow"`
	AuthPluginName  string `json:"auth_plugin_name,omitempty" yaml:"auth_plugin_name,omitempty,flow"`
}

type PluginDetails struct {
	Type    string `json:"type,omitempty" yaml:"type,omitempty,flow"`
	Message string `json:"message,omitempty" yaml:"message,omitempty,flow"`
}

type MySQLHandshakeResponseOk struct {
	PacketIndicator string        `json:"packet_indicator,omitempty" yaml:"packet_indicator,omitempty,flow"`
	PluginDetails   PluginDetails `json:"plugin_details,omitempty" yaml:"plugin_details,omitempty,flow"`
	RemainingBytes  string        `json:"remaining_bytes,omitempty" yaml:"remaining_bytes,omitempty,flow"`
}

type MySQLHandshakeResponse struct {
	CapabilityFlags uint32 `json:"capability_flags,omitempty" yaml:"capability_flags,omitempty,flow"`
	MaxPacketSize   uint32 `json:"max_packet_size,omitempty" yaml:"max_packet_size,omitempty,flow"`
	CharacterSet    uint8  `json:"character_set,omitempty" yaml:"character_set,omitempty,flow"`
	Reserved        int    `json:"reserved,omitempty" yaml:"reserved,omitempty,flow"`
	Username        string `json:"username,omitempty" yaml:"username,omitempty,flow"`
	AuthData        string `json:"auth_data,omitempty" yaml:"auth_data,omitempty,flow"`
	Database        string `json:"database,omitempty" yaml:"database,omitempty,flow"`
	AuthPluginName  string `json:"auth_plugin_name,omitempty" yaml:"auth_plugin_name,omitempty,flow"`
}

type MySQLQueryPacket struct {
	Command byte   `json:"command,omitempty" yaml:"command,omitempty,flow"`
	Query   string `json:"query,omitempty" yaml:"query,omitempty,flow"`
}

type MySQLComStmtExecute struct {
	StatementID    uint32           `json:"statement_id,omitempty" yaml:"statement_id,omitempty,flow"`
	Flags          byte             `json:"flags,omitempty" yaml:"flags,omitempty,flow"`
	IterationCount uint32           `json:"iteration_count,omitempty" yaml:"iteration_count,omitempty,flow"`
	NullBitmap     string           `json:"null_bitmap,omitempty" yaml:"null_bitmap,omitempty,flow"`
	ParamCount     uint16           `json:"param_count,omitempty" yaml:"param_count,omitempty,flow"`
	Parameters     []BoundParameter `json:"parameters,omitempty" yaml:"parameters,omitempty,flow"`
}

type BoundParameter struct {
	Type     byte   `json:"type,omitempty" yaml:"type,omitempty,flow"`
	Unsigned byte   `json:"unsigned,omitempty" yaml:"unsigned,omitempty,flow"`
	Value    []byte `json:"value,omitempty" yaml:"value,omitempty,flow"`
}

type MySQLStmtPrepareOk struct {
	Status       byte               `json:"status,omitempty" yaml:"status,omitempty,flow"`
	StatementID  uint32             `json:"statement_id,omitempty" yaml:"statement_id,omitempty,flow"`
	NumColumns   uint16             `json:"num_columns,omitempty" yaml:"num_columns,omitempty,flow"`
	NumParams    uint16             `json:"num_params,omitempty" yaml:"num_params,omitempty,flow"`
	WarningCount uint16             `json:"warning_count,omitempty" yaml:"warning_count,omitempty,flow"`
	ColumnDefs   []ColumnDefinition `json:"column_definitions,omitempty" yaml:"column_definitions,omitempty,flow"`
	ParamDefs    []ColumnDefinition `json:"param_definitions,omitempty" yaml:"param_definitions,omitempty,flow"`
}

type MySQLResultSet struct {
	Columns             []*ColumnDefinition `json:"columns,omitempty" yaml:"columns,omitempty,flow"`
	Rows                []*Row              `json:"rows,omitempty" yaml:"rows,omitempty,flow"`
	EOFPresent          bool                `json:"eofPresent,omitempty" yaml:"eofPresent,omitempty,flow"`
	PaddingPresent      bool                `json:"paddingPresent,omitempty" yaml:"paddingPresent,omitempty,flow"`
	EOFPresentFinal     bool                `json:"eofPresentFinal,omitempty" yaml:"eofPresentFinal,omitempty,flow"`
	PaddingPresentFinal bool                `json:"paddingPresentFinal,omitempty" yaml:"paddingPresentFinal,omitempty,flow"`
	OptionalPadding     bool                `json:"optionalPadding,omitempty" yaml:"optionalPadding,omitempty,flow"`
	OptionalEOFBytes    string              `json:"optionalEOFBytes,omitempty" yaml:"optionalEOFBytes,omitempty,flow"`
	EOFAfterColumns     string              `json:"eofAfterColumns,omitempty" yaml:"eofAfterColumns,omitempty,flow"`
}

type PacketHeader struct {
	PacketLength     uint8 `json:"packet_length,omitempty" yaml:"packet_length,omitempty,flow"`
	PacketSequenceId uint8 `json:"packet_sequence_id,omitempty" yaml:"packet_sequence_id,omitempty,flow"`
}

type RowHeader struct {
	PacketLength     uint8 `json:"packet_length,omitempty" yaml:"packet_length,omitempty,flow"`
	PacketSequenceId uint8 `json:"packet_sequence_id,omitempty" yaml:"packet_sequence_id,omitempty,flow"`
}

type ColumnDefinition struct {
	Catalog      string       `json:"catalog,omitempty" yaml:"catalog,omitempty,flow"`
	Schema       string       `json:"schema,omitempty" yaml:"schema,omitempty,flow"`
	Table        string       `json:"table,omitempty" yaml:"table,omitempty,flow"`
	OrgTable     string       `json:"org_table,omitempty" yaml:"org_table,omitempty,flow"`
	Name         string       `json:"name,omitempty" yaml:"name,omitempty,flow"`
	OrgName      string       `json:"org_name,omitempty" yaml:"org_name,omitempty,flow"`
	NextLength   uint64       `json:"next_length,omitempty" yaml:"next_length,omitempty,flow"`
	CharacterSet uint16       `json:"character_set,omitempty" yaml:"character_set,omitempty,flow"`
	ColumnLength uint32       `json:"column_length,omitempty" yaml:"column_length,omitempty,flow"`
	ColumnType   byte         `json:"column_type,omitempty" yaml:"column_type,omitempty,flow"`
	Flags        uint16       `json:"flags,omitempty" yaml:"flags,omitempty,flow"`
	Decimals     byte         `json:"decimals,omitempty" yaml:"decimals,omitempty,flow"`
	PacketHeader PacketHeader `json:"packet_header,omitempty" yaml:"packet_header,omitempty,flow"`
}

type Row struct {
	Header  RowHeader             `json:"header,omitempty" yaml:"header,omitempty,flow"`
	Columns []RowColumnDefinition `json:"columns,omitempty" yaml:"row_column_definition,omitempty,flow"`
}

type MySQLOKPacket struct {
	AffectedRows uint64 `json:"affected_rows,omitempty" yaml:"affected_rows,omitempty,flow"`
	LastInsertID uint64 `json:"last_insert_id,omitempty" yaml:"last_insert_id,omitempty,flow"`
	StatusFlags  uint16 `json:"status_flags,omitempty" yaml:"status_flags,omitempty,flow"`
	Warnings     uint16 `json:"warnings,omitempty" yaml:"warnings,omitempty,flow"`
	Info         string `json:"info,omitempty" yaml:"info,omitempty,flow"`
}

type MySQLERRPacket struct {
	Header         byte   `json:"header,omitempty" yaml:"header,omitempty,flow"`
	ErrorCode      uint16 `json:"error_code,omitempty" yaml:"error_code,omitempty,flow"`
	SQLStateMarker string `json:"sql_state_marker,omitempty" yaml:"sql_state_marker,omitempty,flow"`
	SQLState       string `json:"sql_state,omitempty" yaml:"sql_state,omitempty,flow"`
	ErrorMessage   string `json:"error_message,omitempty" yaml:"error_message,omitempty,flow"`
}

type MySQLComStmtPreparePacket struct {
	Query string `json:"query,omitempty" yaml:"query,omitempty,flow"`
}

type MySQLCOM_STMT_SEND_LONG_DATA struct {
	StatementID uint32 `json:"statement_id,omitempty" yaml:"statement_id,omitempty,flow"`
	ParameterID uint16 `json:"parameter_id,omitempty" yaml:"parameter_id,omitempty,flow"`
	Data        string `json:"data,omitempty" yaml:"data,omitempty,flow"`
}

type MySQLCOM_STMT_RESET struct {
	StatementID uint32 `json:"statement_id,omitempty" yaml:"statement_id,omitempty,flow"`
}

type MySQLComStmtFetchPacket struct {
	StatementID uint32 `json:"statement_id,omitempty" yaml:"statement_id,omitempty,flow"`
	RowCount    uint32 `json:"row_count,omitempty" yaml:"row_count,omitempty,flow"`
	Info        string `json:"info,omitempty" yaml:"info"`
}

type MySQLComChangeUserPacket struct {
	User         string `json:"user,omitempty" yaml:"user,omitempty,flow"`
	Auth         string `json:"auth,omitempty" yaml:"auth,omitempty,flow"`
	Db           string `json:"db,omitempty" yaml:"db,omitempty,flow"`
	CharacterSet uint8  `json:"character_set,omitempty" yaml:"character_set,omitempty,flow"`
	AuthPlugin   string `json:"auth_plugin,omitempty" yaml:"auth_plugin,omitempty,flow"`
}

type MySQLComStmtClosePacket struct {
	StatementID uint32 `json:"statement_id,omitempty" yaml:"statement_id,omitempty,flow"`
}

type AuthSwitchResponsePacket struct {
	AuthResponseData string `json:"auth_response_data,omitempty" yaml:"auth_response_data,omitempty,flow"`
}

type AuthSwitchRequestPacket struct {
	StatusTag      byte   `json:"status_tag,omitempty" yaml:"status_tag,omitempty,flow"`
	PluginName     string `json:"plugin_name,omitempty" yaml:"plugin_name,omitempty,flow"`
	PluginAuthData string `json:"plugin_authdata,omitempty" yaml:"plugin_authdata,omitempty,flow"`
}
