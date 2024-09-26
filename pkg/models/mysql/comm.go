// Package mysql in models provides related structs for mysql protocol
package mysql

// This file contains struct for command phase packets
/* refer:

(i) https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_command_phase_text.html
(ii) https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_command_phase_utility.html
(iii) https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_command_phase_ps.html

*/

// COM_QUERY packet (currently does not support if CLIENT_QUERY_ATTRIBUTES is set)

type QueryPacket struct {
	Command byte   `yaml:"command" json:"command"`
	Query   string `yaml:"query" json:"query"`
}

// LocalInFileRequestPacket is used to send local file request to server, currently not supported
type LocalInFileRequestPacket struct {
	PacketType byte   `yaml:"command" json:"command"`
	Filename   string `yaml:"filename" json:"filename"`
}

// TextResultSet is used as a response packet for COM_QUERY
type TextResultSet struct {
	ColumnCount     uint64                `yaml:"columnCount" json:"columnCount"`
	Columns         []*ColumnDefinition41 `yaml:"columns" json:"columns"`
	EOFAfterColumns []byte                `yaml:"eofAfterColumns" json:"eofAfterColumns"`
	Rows            []*TextRow            `yaml:"rows" json:"rows"`
	FinalResponse   *GenericResponse      `yaml:"FinalResponse" json:"FinalResponse"`
}

// BinaryProtocolResultSet is used as a response packet for COM_STMT_EXECUTE
type BinaryProtocolResultSet struct {
	ColumnCount     uint64                `yaml:"columnCount" json:"columnCount"`
	Columns         []*ColumnDefinition41 `yaml:"columns" json:"columns"`
	EOFAfterColumns []byte                `yaml:"eofAfterColumns" json:"eofAfterColumns"`
	Rows            []*BinaryRow          `yaml:"rows" json:"rows"`
	FinalResponse   *GenericResponse      `yaml:"FinalResponse" json:"FinalResponse"`
}

type GenericResponse struct {
	Data []byte `yaml:"data" json:"data"`
	Type string `yaml:"type" json:"type"`
}

// Columns

type ColumnCount struct {
	// Header    Header `yaml:"header" json:"header"`
	Count uint64 `yaml:"count" json:"count"`
}

type ColumnDefinition41 struct {
	Header       Header `yaml:"header" json:"header"`
	Catalog      string `yaml:"catalog" json:"catalog"`
	Schema       string `yaml:"schema" json:"schema"`
	Table        string `yaml:"table" json:"table"`
	OrgTable     string `yaml:"org_table" json:"org_table"`
	Name         string `yaml:"name" json:"name"`
	OrgName      string `yaml:"org_name" json:"org_name"`
	FixedLength  byte   `yaml:"fixed_length" json:"fixed_length"`
	CharacterSet uint16 `yaml:"character_set" json:"character_set"`
	ColumnLength uint32 `yaml:"column_length" json:"column_length"`
	Type         byte   `yaml:"type" json:"type"`
	Flags        uint16 `yaml:"flags" json:"flags"`
	Decimals     byte   `yaml:"decimals" json:"decimals"`
	Filler       []byte `yaml:"filler" json:"filler"`
	DefaultValue string `yaml:"defaultValue" json:"defaultValue"`
}

// Rows

type TextRow struct {
	Header Header        `yaml:"header" json:"header"`
	Values []ColumnEntry `yaml:"values" json:"values"`
}

type BinaryRow struct {
	Header        Header        `yaml:"header" json:"header"`
	Values        []ColumnEntry `yaml:"values" json:"values"`
	OkAfterRow    bool          `yaml:"okAfterRow" json:"okAfterRow"`
	RowNullBuffer []byte        `yaml:"rowNullBuffer" json:"rowNullBuffer"`
}

type ColumnEntry struct {
	Type     FieldType   `yaml:"type" json:"type"`
	Name     string      `yaml:"name" json:"name"`
	Value    interface{} `yaml:"value" json:"value"`
	Unsigned bool        `yaml:"unsigned" json:"unsigned"`
}

// COM_STMT_PREPARE packet

type StmtPreparePacket struct {
	Command byte   `yaml:"command" json:"command"`
	Query   string `yaml:"query" json:"query"`
}

// COM_STMT_PREPARE_OK packet

type StmtPrepareOkPacket struct {
	Status       byte   `yaml:"status" json:"status"`
	StatementID  uint32 `yaml:"statement_id" json:"statement_id"`
	NumColumns   uint16 `yaml:"num_columns" json:"num_columns"`
	NumParams    uint16 `yaml:"num_params" json:"num_params"`
	Filler       byte   `yaml:"filler" json:"filler"`
	WarningCount uint16 `yaml:"warning_count" json:"warning_count"`

	ParamDefs          []*ColumnDefinition41 `yaml:"param_definitions" json:"param_definitions"`
	EOFAfterParamDefs  []byte                `yaml:"eofAfterParamDefs" json:"eofAfterParamDefs"`
	ColumnDefs         []*ColumnDefinition41 `yaml:"column_definitions" json:"column_definitions"`
	EOFAfterColumnDefs []byte                `yaml:"eofAfterColumnDefs" json:"eofAfterColumnDefs"`
}

// COM_STMT_EXECUTE packet

type StmtExecutePacket struct {
	Status            byte        `yaml:"status" json:"status"`
	StatementID       uint32      `yaml:"statement_id" json:"statement_id"`
	Flags             byte        `yaml:"flags" json:"flags"`
	IterationCount    uint32      `yaml:"iteration_count" json:"iteration_count"`
	ParameterCount    int         `yaml:"parameter_count" json:"parameter_count"`
	NullBitmap        []byte      `yaml:"null_bitmap" json:"null_bitmap"`
	NewParamsBindFlag byte        `yaml:"new_params_bind_flag" json:"new_params_bind_flag"`
	Parameters        []Parameter `yaml:"parameters" json:"parameters"`
}

type Parameter struct {
	Type     uint16 `yaml:"type" json:"type"`
	Unsigned bool   `yaml:"unsigned" json:"unsigned"`
	Name     string `yaml:"name,omitempty" json:"name,omitempty"`
	Value    []byte `yaml:"value" json:"value"`
}

// COM_STMT_FETCH packet is not currently supported because its response involves multi-resultset

type StmtFetchPacket struct {
	Status      byte   `yaml:"status" json:"status"`
	StatementID uint32 `yaml:"statement_id" json:"statement_id"`
	NumRows     uint32 `yaml:"num_rows" json:"num_rows"`
}

// COM_STMT_CLOSE packet

type StmtClosePacket struct {
	Status      byte   `yaml:"status" json:"status"`
	StatementID uint32 `yaml:"statement_id" json:"statement_id"`
}

// COM_STMT_RESET packet

type StmtResetPacket struct {
	Status      byte   `yaml:"status" json:"status"`
	StatementID uint32 `yaml:"statement_id" json:"statement_id"`
}

// COM_STMT_SEND_LONG_DATA packet

type StmtSendLongDataPacket struct {
	Status      byte   `yaml:"status" json:"status"`
	StatementID uint32 `yaml:"statement_id" json:"statement_id"`
	ParameterID uint16 `yaml:"parameter_id" json:"parameter_id"`
	Data        []byte `yaml:"data" json:"data"`
}

// Utility commands

// COM_QUIT packet

type QuitPacket struct {
	Command byte `yaml:"command" json:"command"`
}

// COM_INIT_DB packet

type InitDBPacket struct {
	Command byte   `yaml:"command" json:"command"`
	Schema  string `yaml:"schema" json:"schema"`
}

// COM_STATISTICS packet

type StatisticsPacket struct {
	Command byte `yaml:"command" json:"command"`
}

// COM_DEBUG packet

type DebugPacket struct {
	Command byte `yaml:"command" json:"command"`
}

// COM_PING packet

type PingPacket struct {
	Command byte `yaml:"command" json:"command"`
}

// COM_RESET_CONNECTION packet

type ResetConnectionPacket struct {
	Command byte `yaml:"command" json:"command"`
}

// COM_SET_OPTION packet

type SetOptionPacket struct {
	Status byte   `yaml:"status" json:"status"`
	Option uint16 `yaml:"option" json:"option"`
}

// COM_CHANGE_USER packet (Not completed/supported as of now)
//refer: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_change_user.html

type ChangeUserPacket struct {
	Command byte `yaml:"command" json:"command"`
	// rest of the fields are not present as the packet is not supported
}
