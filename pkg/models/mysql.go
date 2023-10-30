package models

type MySQLPacketHeader struct {
	PacketLength uint32 `json:"packet_length" yaml:"packet_length"`
	PacketNumber uint8  `json:"packet_number" yaml:"packet_number"`
	PacketType   string `json:"packet_type" yaml:"packet_type"`
}
type MySQLRequest struct {
	Header    *MySQLPacketHeader `json:"header" yaml:"header"`
	Message   interface{}        `json:"message" yaml:"message"`
	ReadDelay int64              `json:"read_delay,omitempty"`
}
type RowColumnDefinition struct {
	Type  FieldType   `yaml:"type"`
	Name  string      `yaml:"name"`
	Value interface{} `yaml:"value"`
}
type MySQLResponse struct {
	Header    *MySQLPacketHeader `json:"header" yaml:"header"`
	Message   interface{}        `json:"message" yaml:"message"`
	ReadDelay int64              `json:"read_delay,omitempty"`
}

type MySQLHandshakeV10Packet struct {
	ProtocolVersion uint8  `yaml:"protocol_version"`
	ServerVersion   string `yaml:"server_version"`
	ConnectionID    uint32 `yaml:"connection_id"`
	AuthPluginData  []byte `yaml:"auth_plugin_data"`
	CapabilityFlags uint32 `yaml:"capability_flags"`
	CharacterSet    uint8  `yaml:"character_set"`
	StatusFlags     uint16 `yaml:"status_flags"`
	AuthPluginName  string `yaml:"auth_plugin_name"`
}

type PluginDetails struct {
	Type    string `yaml:"type"`
	Message string `yaml:"message"`
}
type MySQLHandshakeResponseOk struct {
	PacketIndicator string        `yaml:"packet_indicator"`
	PluginDetails   PluginDetails `yaml:"plugin_details"`
	RemainingBytes  []byte        `yaml:"remaining_bytes"`
}
type MySQLHandshakeResponse struct {
	CapabilityFlags uint32   `yaml:"capability_flags"`
	MaxPacketSize   uint32   `yaml:"max_packet_size"`
	CharacterSet    uint8    `yaml:"character_set"`
	Reserved        [23]byte `yaml:"reserved"`
	Username        string   `yaml:"username"`
	AuthData        []byte   `yaml:"auth_data"`
	Database        string   `yaml:"database"`
	AuthPluginName  string   `yaml:"auth_plugin_name"`
}

type MySQLQueryPacket struct {
	Command byte   `yaml:"command"`
	Query   string `yaml:"query"`
}

type MySQLComStmtExecute struct {
	StatementID    uint32           `yaml:"statement_id"`
	Flags          byte             `yaml:"flags"`
	IterationCount uint32           `yaml:"iteration_count"`
	NullBitmap     []byte           `yaml:"null_bitmap"`
	ParamCount     uint16           `yaml:"param_count"`
	Parameters     []BoundParameter `yaml:"parameters"`
}
type BoundParameter struct {
	Type     byte   `yaml:"type"`
	Unsigned byte   `yaml:"unsigned"`
	Value    []byte `yaml:"value"`
}

type MySQLStmtPrepareOk struct {
	Status       byte               `yaml:"status"`
	StatementID  uint32             `yaml:"statement_id"`
	NumColumns   uint16             `yaml:"num_columns"`
	NumParams    uint16             `yaml:"num_params"`
	WarningCount uint16             `yaml:"warning_count"`
	ColumnDefs   []ColumnDefinition `yaml:"column_definitions"`
	ParamDefs    []ColumnDefinition `yaml:"param_definitions"`
}

type MySQLResultSet struct {
	Columns []*ColumnDefinition `yaml:"columns"`
	Rows    []*Row              `yaml:"rows"`
}
type PacketHeader struct {
	PacketLength     uint8 `yaml:"packet_length"`
	PacketSequenceId uint8 `yaml:"packet_sequence_id"`
}
type RowHeader struct {
	PacketLength     uint8 `yaml:"packet_length"`
	PacketSequenceId uint8 `yaml:"packet_sequence_id"`
}
type ColumnDefinition struct {
	Catalog      string       `yaml:"catalog"`
	Schema       string       `yaml:"schema"`
	Table        string       `yaml:"table"`
	OrgTable     string       `yaml:"org_table"`
	Name         string       `yaml:"name"`
	OrgName      string       `yaml:"org_name"`
	NextLength   uint64       `yaml:"next_length"`
	CharacterSet uint16       `yaml:"character_set"`
	ColumnLength uint32       `yaml:"column_length"`
	ColumnType   byte         `yaml:"column_type"`
	Flags        uint16       `yaml:"flags"`
	Decimals     byte         `yaml:"decimals"`
	PacketHeader PacketHeader `yaml:"packet_header"`
}

type Row struct {
	Header  RowHeader             `yaml:"header"`
	Columns []RowColumnDefinition `yaml:"row_column_definition"`
}

type MySQLOKPacket struct {
	AffectedRows uint64 `json:"affected_rows,omitempty" yaml:"affected_rows"`
	LastInsertID uint64 `json:"last_insert_id,omitempty" yaml:"last_insert_id"`
	StatusFlags  uint16 `json:"status_flags,omitempty" yaml:"status_flags"`
	Warnings     uint16 `json:"warnings,omitempty" yaml:"warnings"`
	Info         string `json:"info,omitempty" yaml:"info"`
}

type MySQLERRPacket struct {
	Header         byte   `yaml:"header"`
	ErrorCode      uint16 `yaml:"error_code"`
	SQLStateMarker string `yaml:"sql_state_marker"`
	SQLState       string `yaml:"sql_state"`
	ErrorMessage   string `yaml:"error_message"`
}

type MySQLComStmtPreparePacket struct {
	Query string
}

type MySQLCOM_STMT_SEND_LONG_DATA struct {
	StatementID uint32 `yaml:"statement_id"`
	ParameterID uint16 `yaml:"parameter_id"`
	Data        []byte `yaml:"data"`
}

type MySQLCOM_STMT_RESET struct {
	StatementID uint32 `yaml:"statement_id"`
}

type MySQLComStmtFetchPacket struct {
	StatementID uint32 `yaml:"statement_id"`
	RowCount    uint32 `yaml:"row_count"`
	Info        string `yaml:"info"`
}

type MySQLComChangeUserPacket struct {
	User         string `yaml:"user"`
	Auth         []byte `yaml:"auth"`
	Db           string `yaml:"db"`
	CharacterSet uint8  `yaml:"character_set"`
	AuthPlugin   string `yaml:"auth_plugin"`
}

type MySQLComStmtClosePacket struct {
	StatementID uint32
}
type AuthSwitchResponsePacket struct {
	AuthResponseData string `yaml:"auth_response_data"`
}
type AuthSwitchRequestPacket struct {
	StatusTag      byte   `yaml:"status_tag"`
	PluginName     string `yaml:"plugin_name"`
	PluginAuthData string `yaml:"plugin_authdata"`
}
