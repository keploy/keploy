package models

type MySQLPacketHeader struct {
	PacketLength uint32 `json:"packet_length" yaml:"packet_length" bson:"packet_length"`
	PacketNumber uint8  `json:"packet_number" yaml:"packet_number" bson:"packet_number"`
	PacketType   string `json:"packet_type" yaml:"packet_type" bson:"packet_type"`
}

type MySQLRequest struct {
	Header    *MySQLPacketHeader `json:"header" yaml:"header" bson:"header"`
	Message   interface{}        `json:"message" yaml:"message" bson:"message"`
	ReadDelay int64              `json:"read_delay,omitempty" yaml:"read_delay" bson:"read_delay,omitempty"`
}
type RowColumnDefinition struct {
	Type  FieldType   `yaml:"type" bson:"type"`
	Name  string      `yaml:"name" bson:"name"`
	Value interface{} `yaml:"value" bson:"value"`
}

type MySQLResponse struct {
	Header    *MySQLPacketHeader `json:"header" yaml:"header" bson:"header"`
	Message   interface{}        `json:"message" yaml:"message" bson:"message"`
	ReadDelay int64              `json:"read_delay,omitempty" yaml:"read_delay" bson:"read_delay,omitempty"`
}

type MySQLHandshakeV10Packet struct {
	ProtocolVersion uint8  `yaml:"protocol_version" bson:"protocol_version"`
	ServerVersion   string `yaml:"server_version" bson:"server_version"`
	ConnectionID    uint32 `yaml:"connection_id" bson:"connection_id"`
	AuthPluginData  []byte `yaml:"auth_plugin_data" bson:"auth_plugin_data"`
	CapabilityFlags uint32 `yaml:"capability_flags" bson:"capability_flags"`
	CharacterSet    uint8  `yaml:"character_set" bson:"character_set"`
	StatusFlags     uint16 `yaml:"status_flags" bson:"status_flags"`
	AuthPluginName  string `yaml:"auth_plugin_name" bson:"auth_plugin_name"`
}

type PluginDetails struct {
	Type    string `yaml:"type" bson:"type"`
	Message string `yaml:"message" bson:"message"`
}

type MySQLHandshakeResponseOk struct {
	PacketIndicator string        `yaml:"packet_indicator" bson:"packet_indicator"`
	PluginDetails   PluginDetails `yaml:"plugin_details" bson:"plugin_details"`
	RemainingBytes  []byte        `yaml:"remaining_bytes" bson:"remaining_bytes"`
}

type MySQLHandshakeResponse struct {
	CapabilityFlags uint32   `yaml:"capability_flags" bson:"capability_flags"`
	MaxPacketSize   uint32   `yaml:"max_packet_size" bson:"max_packet_size"`
	CharacterSet    uint8    `yaml:"character_set" bson:"character_set"`
	Reserved        [23]byte `yaml:"reserved" bson:"reserved"`
	Username        string   `yaml:"username" bson:"username"`
	AuthData        []byte   `yaml:"auth_data" bson:"auth_data"`
	Database        string   `yaml:"database" bson:"database"`
	AuthPluginName  string   `yaml:"auth_plugin_name" bson:"auth_plugin_name"`
}

type MySQLQueryPacket struct {
	Command byte   `yaml:"command" bson:"command"`
	Query   string `yaml:"query" bson:"query"`
}

type MySQLComStmtExecute struct {
	StatementID    uint32           `yaml:"statement_id" bson:"statement_id"`
	Flags          byte             `yaml:"flags" bson:"flags"`
	IterationCount uint32           `yaml:"iteration_count" bson:"iteration_count"`
	NullBitmap     []byte           `yaml:"null_bitmap" bson:"null_bitmap"`
	ParamCount     uint16           `yaml:"param_count" bson:"param_count"`
	Parameters     []BoundParameter `yaml:"parameters" bson:"parameters"`
}

type BoundParameter struct {
	Type     byte   `yaml:"type" bson:"type"`
	Unsigned byte   `yaml:"unsigned" bson:"unsigned"`
	Value    []byte `yaml:"value" bson:"value"`
}

type MySQLStmtPrepareOk struct {
	Status       byte               `yaml:"status" bson:"status"`
	StatementID  uint32             `yaml:"statement_id" bson:"statement_id"`
	NumColumns   uint16             `yaml:"num_columns" bson:"num_columns"`
	NumParams    uint16             `yaml:"num_params" bson:"num_params"`
	WarningCount uint16             `yaml:"warning_count" bson:"warning_count"`
	ColumnDefs   []ColumnDefinition `yaml:"column_definitions" bson:"column_definitions"`
	ParamDefs    []ColumnDefinition `yaml:"param_definitions" bson:"param_definitions"`
}

type MySQLResultSet struct {
	Columns []*ColumnDefinition `yaml:"columns" bson:"columns"`
	Rows    []*Row              `yaml:"rows" bson:"rows"`
}

type PacketHeader struct {
	PacketLength     uint8 `yaml:"packet_length" bson:"packet_length"`
	PacketSequenceId uint8 `yaml:"packet_sequence_id" bson:"packet_sequence_id"`
}

type RowHeader struct {
	PacketLength     uint8 `yaml:"packet_length" bson:"packet_length"`
	PacketSequenceId uint8 `yaml:"packet_sequence_id" bson:"packet_sequence_id"`
}

type ColumnDefinition struct {
	Catalog      string       `yaml:"catalog" bson:"catalog"`
	Schema       string       `yaml:"schema" bson:"schema"`
	Table        string       `yaml:"table" bson:"table"`
	OrgTable     string       `yaml:"org_table" bson:"org_table"`
	Name         string       `yaml:"name" bson:"name"`
	OrgName      string       `yaml:"org_name" bson:"org_name"`
	NextLength   uint64       `yaml:"next_length" bson:"next_length"`
	CharacterSet uint16       `yaml:"character_set" bson:"character_set"`
	ColumnLength uint32       `yaml:"column_length" bson:"column_length"`
	ColumnType   byte         `yaml:"column_type" bson:"column_type"`
	Flags        uint16       `yaml:"flags" bson:"flags"`
	Decimals     byte         `yaml:"decimals" bson:"decimals"`
	PacketHeader PacketHeader `yaml:"packet_header" bson:"packet_header"`
}

type Row struct {
	Header  RowHeader             `yaml:"header" bson:"header"`
	Columns []RowColumnDefinition `yaml:"row_column_definition" bson:"row_column_definition"`
}

type MySQLOKPacket struct {
	AffectedRows uint64 `json:"affected_rows,omitempty" yaml:"affected_rows" bson:"affected_rows,omitempty"`
	LastInsertID uint64 `json:"last_insert_id,omitempty" yaml:"last_insert_id" bson:"last_insert_id,omitempty"`
	StatusFlags  uint16 `json:"status_flags,omitempty" yaml:"status_flags" bson:"status_flags,omitempty"`
	Warnings     uint16 `json:"warnings,omitempty" yaml:"warnings" bson:"warnings,omitempty"`
	Info         string `json:"info,omitempty" yaml:"info" bson:"info,omitempty"`
}

type MySQLERRPacket struct {
	Header         byte   `yaml:"header" bson:"header"`
	ErrorCode      uint16 `yaml:"error_code" bson:"error_code"`
	SQLStateMarker string `yaml:"sql_state_marker" bson:"sql_state_marker"`
	SQLState       string `yaml:"sql_state" bson:"sql_state"`
	ErrorMessage   string `yaml:"error_message" bson:"error_message"`
}

type MySQLComStmtPreparePacket struct {
	Query string `bson:"query"`
}

type MySQLCOM_STMT_SEND_LONG_DATA struct {
	StatementID uint32 `yaml:"statement_id" bson:"statement_id"`
	ParameterID uint16 `yaml:"parameter_id" bson:"parameter_id"`
	Data        []byte `yaml:"data" bson:"data"`
}

type MySQLCOM_STMT_RESET struct {
	StatementID uint32 `yaml:"statement_id" bson:"statement_id"`
}

type MySQLComStmtFetchPacket struct {
	StatementID uint32 `yaml:"statement_id" bson:"statement_id"`
	RowCount    uint32 `yaml:"row_count" bson:"row_count"`
	Info        string `yaml:"info" bson:"info"`
}

type MySQLComChangeUserPacket struct {
	User         string `yaml:"user" bson:"user"`
	Auth         []byte `yaml:"auth" bson:"auth"`
	Db           string `yaml:"db" bson:"db"`
	CharacterSet uint8  `yaml:"character_set" bson:"character_set"`
	AuthPlugin   string `yaml:"auth_plugin" bson:"auth_plugin"`
}

type MySQLComStmtClosePacket struct {
	StatementID uint32 `bson:"statement_id"`
}

type AuthSwitchResponsePacket struct {
	AuthResponseData string `yaml:"auth_response_data" bson:"auth_response_data"`
}

type AuthSwitchRequestPacket struct {
	StatusTag      byte   `yaml:"status_tag" bson:"status_tag"`
	PluginName     string `yaml:"plugin_name" bson:"plugin_name"`
	PluginAuthData string `yaml:"plugin_authdata" bson:"plugin_authdata"`
}
