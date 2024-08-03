package mysql

// This file contains struct for command phase packets
/* refer:

(i) https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_command_phase_text.html
(ii) https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_command_phase_utility.html
(iii) https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_command_phase_ps.html

*/

// COM_QUERY packet (currently does not support if CLIENT_QUERY_ATTRIBUTES is set)

type QueryPacket struct {
	Command byte   `yaml:"command"`
	Query   string `yaml:"query"`
}

// LocalInFileRequestPacket is used to send local file request to server, currently not supported
type LocalInFileRequestPacket struct {
	PacketType byte `yaml:"command"`
	Filename   string
}

// TextResultSet is used as a response packet for COM_QUERY
type TextResultSet struct {
	Count           *ColumnCount          `yaml:"columnCount"`
	Columns         []*ColumnDefinition41 `yaml:"columns"`
	EOFAfterColumns []byte                `yaml:"eofAfterColumns"`
	Rows            []*TextRow            `yaml:"rows"`
	EOFAfterRows    []byte                `yaml:"eofAfterRows"`
}

// BinaryProtocolResultSet is used as a response packet for COM_STMT_EXECUTE
type BinaryProtocolResultSet struct {
	Count           *ColumnCount          `yaml:"columnCount"`
	Columns         []*ColumnDefinition41 `yaml:"columns"`
	EOFAfterColumns []byte                `yaml:"eofAfterColumns"`
	Rows            []*BinaryRow          `yaml:"rows"`
	EOFAfterRows    []byte                `yaml:"eofAfterRows"`
}

// Columns
type ColumnCount struct {
	Header    Header `yaml:"header"`
	ColumnNum uint64 `yaml:"columnNum"`
}
type ColumnDefinition41 struct {
	Header       Header `yaml:"header"`
	Catalog      string `yaml:"catalog"`
	Schema       string `yaml:"schema"`
	Table        string `yaml:"table"`
	OrgTable     string `yaml:"org_table"`
	Name         string `yaml:"name"`
	OrgName      string `yaml:"org_name"`
	FixedLength  byte   `yaml:"fixed_length"`
	CharacterSet uint16 `yaml:"character_set"`
	ColumnLength uint32 `yaml:"column_length"`
	Type         byte   `yaml:"type"`
	Flags        uint16 `yaml:"flags"`
	Decimals     byte   `yaml:"decimals"`
	Filler       []byte `yaml:"filler"`
	DefaultValue string `yaml:"defaultValue"`
}

//Rows

type TextRow struct {
	Header Header        `yaml:"header"`
	Values []ColumnEntry `yaml:"values"`
}

type BinaryRow struct {
	Header        Header        `yaml:"header"`
	Values        []ColumnEntry `yaml:"values"`
	OkAfterRow    bool          `yaml:"okAfterRow"`
	RowNullBuffer []byte        `yaml:"rowNullBuffer"`
}

type ColumnEntry struct {
	Type  FieldType   `yaml:"type"`
	Name  string      `yaml:"name"`
	Value interface{} `yaml:"value"`
}

// COM_STMT_PREPARE packet

type StmtPreparePacket struct {
	Command byte   `yaml:"command"`
	Query   string `yaml:"query"`
}

// COM_STMT_PREPARE_OK packet

type StmtPrepareOkPacket struct {
	Status       byte   `yaml:"status"`
	StatementID  uint32 `yaml:"statement_id"`
	NumColumns   uint16 `yaml:"num_columns"`
	NumParams    uint16 `yaml:"num_params"`
	Filler       byte   `yaml:"filler"`
	WarningCount uint16 `yaml:"warning_count"`

	ParamDefs          []ColumnDefinition41 `yaml:"param_definitions"`
	EOFAfterParamDefs  []byte               `yaml:"eofAfterParamDefs"`
	ColumnDefs         []ColumnDefinition41 `yaml:"column_definitions"`
	EOFAfterColumnDefs []byte               `yaml:"eofAfterColumnDefs"`
}

// COM_STMT_EXECUTE packet

type StmtExecutePacket struct {
	Status            byte        `yaml:"status"`
	StatementID       uint32      `yaml:"statement_id"`
	Flags             byte        `yaml:"flags"`
	IterationCount    uint32      `yaml:"iteration_count"`
	ParameterCount    int         `yaml:"parameter_count"`
	NullBitmap        []byte      `yaml:"null_bitmap"`
	NewParamsBindFlag byte        `yaml:"new_params_bind_flag"`
	Parameters        []Parameter `yaml:"parameters"`
}

type Parameter struct {
	Type     uint16 `yaml:"type"`
	Unsigned bool   `yaml:"unsigned"`
	Name     string `yaml:"name,omitempty"`
	Value    []byte `yaml:"value"`
}

// COM_STMT_FETCH packet is not currently supported because its response involves multi-resultset

type StmtFetchPacket struct {
	Status      byte   `yaml:"status"`
	StatementID uint32 `yaml:"statement_id"`
	NumRows     uint32 `yaml:"num_rows"`
}

// COM_STMT_CLOSE packet

type StmtClosePacket struct {
	Status      byte   `yaml:"status"`
	StatementID uint32 `yaml:"statement_id"`
}

// COM_STMT_RESET packet

type StmtResetPacket struct {
	Status      byte   `yaml:"status"`
	StatementID uint32 `yaml:"statement_id"`
}

// COM_STMT_SEND_LONG_DATA packet

type StmtSendLongDataPacket struct {
	Status      byte   `yaml:"status"`
	StatementID uint32 `yaml:"statement_id"`
	ParameterID uint16 `yaml:"parameter_id"`
	Data        []byte `yaml:"data"`
}

// Some merged packets (identified in the wireshark trace)

type StmtCloseAndPreparePacket struct {
	StmtClose     StmtClosePacket
	PrepareHeader Header
	StmtPrepare   StmtPreparePacket
}

type StmtCloseAndQueryPacket struct {
	StmtClose   StmtClosePacket
	QueryHeader Header
	StmtQuery   QueryPacket
}

// Utility commands

// COM_QUIT packet

type QuitPacket struct {
	Command byte `yaml:"command"`
}

// COM_INIT_DB packet

type InitDBPacket struct {
	Command byte   `yaml:"command"`
	Schema  string `yaml:"schema"`
}

// COM_STATISTICS packet

type StatisticsPacket struct {
	Command byte `yaml:"command"`
}

// COM_DEBUG packet

type DebugPacket struct {
	Command byte `yaml:"command"`
}

// COM_PING packet

type PingPacket struct {
	Command byte `yaml:"command"`
}

// COM_RESET_CONNECTION packet

type ResetConnectionPacket struct {
	Command byte `yaml:"command"`
}

// COM_SET_OPTION packet

type SetOptionPacket struct {
	Status byte   `yaml:"status"`
	Option uint16 `yaml:"option"`
}

// COM_CHANGE_USER packet (Not completed/supported as of now)
//refer: https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_com_change_user.html

type ChangeUserPacket struct {
	Command byte `yaml:"command"`
	// rest of the fields are not present as the packet is not supported
}
