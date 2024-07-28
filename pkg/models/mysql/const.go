package mysql

// Basic Response Packet Status
const (
	OK  byte = 0x00
	ERR byte = 0xff
	EOF byte = 0xfe
)

// LocalInFile Request packet type is not supported
const LocalInFile = 0xfb

// Auth Packet Status
const (
	AuthSwitchRequest   byte = 0xfe
	AuthMoreData        byte = 0x01
	AuthNextFactor      byte = 0x02
	HandshakeV10        byte = 0x0a
	HandshakeResponse41 byte = 0x8d
)

type CachingSha2Password byte

// CachingSha2Password constants
const (
	RequestPublicKey          CachingSha2Password = 2
	FastAuthSuccess           CachingSha2Password = 3
	PerformFullAuthentication CachingSha2Password = 4
)

// Client Capability Flags
const (
	// https://dev.mysql.com/doc/dev/mysql-server/latest/group__group__cs__capabilities__flags.html

	CLIENT_LONG_PASSWORD uint32 = 1 << iota
	CLIENT_FOUND_ROWS
	CLIENT_LONG_FLAG
	CLIENT_CONNECT_WITH_DB
	CLIENT_NO_SCHEMA
	CLIENT_COMPRESS
	CLIENT_ODBC
	CLIENT_LOCAL_FILES
	CLIENT_IGNORE_SPACE
	CLIENT_PROTOCOL_41
	CLIENT_INTERACTIVE
	CLIENT_SSL
	CLIENT_IGNORE_SIGPIPE
	CLIENT_TRANSACTIONS
	CLIENT_RESERVED
	CLIENT_SECURE_CONNECTION
	CLIENT_MULTI_STATEMENTS
	CLIENT_MULTI_RESULTS
	CLIENT_PS_MULTI_RESULTS
	CLIENT_PLUGIN_AUTH
	CLIENT_CONNECT_ATTRS
	CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA
	CLIENT_CAN_HANDLE_EXPIRED_PASSWORDS
	CLIENT_SESSION_TRACK
	CLIENT_DEPRECATE_EOF
	CLIENT_OPTIONAL_RESULTSET_METADATA
	CLIENT_ZSTD_COMPRESSION_ALGORITHM
	CLIENT_QUERY_ATTRIBUTES
	MULTI_FACTOR_AUTHENTICATION
	CLIENT_CAPABILITY_EXTENSION
	CLIENT_SSL_VERIFY_SERVER_CERT
	CLIENT_REMEMBER_OPTIONS
)

type FieldType byte

// Field Types
const (
	FieldTypeDecimal FieldType = iota
	FieldTypeTiny
	FieldTypeShort
	FieldTypeLong
	FieldTypeFloat
	FieldTypeDouble
	FieldTypeNULL
	FieldTypeTimestamp
	FieldTypeLongLong
	FieldTypeInt24
	FieldTypeDate
	FieldTypeTime
	FieldTypeDateTime
	FieldTypeYear
	FieldTypeNewDate
	FieldTypeVarChar
	FieldTypeBit
)

// Additional Field Types
const (
	FieldTypeJSON FieldType = iota + 0xf5
	FieldTypeNewDecimal
	FieldTypeEnum
	FieldTypeSet
	FieldTypeTinyBLOB
	FieldTypeMediumBLOB
	FieldTypeLongBLOB
	FieldTypeBLOB
	FieldTypeVarString
	FieldTypeString
	FieldTypeGeometry
)

// Field Flags
const (
	NOT_NULL_FLAG       = 1
	PRI_KEY_FLAG        = 2
	UNIQUE_KEY_FLAG     = 4
	BLOB_FLAG           = 16
	UNSIGNED_FLAG       = 32
	ZEROFILL_FLAG       = 64
	BINARY_FLAG         = 128
	ENUM_FLAG           = 256
	AUTO_INCREMENT_FLAG = 512
	TIMESTAMP_FLAG      = 1024
	SET_FLAG            = 2048
	NUM_FLAG            = 32768
	PART_KEY_FLAG       = 16384
	GROUP_FLAG          = 32768
	UNIQUE_FLAG         = 65536
)

// Utility command Packet Status
const (
	COM_QUIT             byte = 0x01
	COM_INIT_DB          byte = 0x02
	COM_FIELD_LIST       byte = 0x04
	COM_STATISTICS       byte = 0x08
	COM_DEBUG            byte = 0x0d
	COM_PING             byte = 0x0e
	COM_CHANGE_USER      byte = 0x11
	COM_RESET_CONNECTION byte = 0x1f
	// COM_SET_OPTION       byte = 0x1a
)

// Command Packet Status
const (
	COM_QUERY        byte = 0x03
	COM_STMT_PREPARE byte = 0x16
	COM_STMT_EXECUTE byte = 0x17
	// COM_STMT_FETCH          byte = 0x19
	COM_STMT_CLOSE          byte = 0x19
	COM_STMT_RESET          byte = 0x1a
	COM_STMT_SEND_LONG_DATA byte = 0x18
)

// ResultSet packets
type ResultSet string

// ResultSet types
const (
	Binary ResultSet = "BinaryProtocolResultSet"
	Text   ResultSet = "TextResultSet"
)

// Define the maps for basic response packets
var statusToString = map[byte]string{
	OK:          "OK",
	ERR:         "ERR",
	EOF:         "EOF",
	LocalInFile: "LocalInFile",
}

// Define the maps for auth packet status
var authStatusToString = map[byte]string{
	AuthSwitchRequest:   "AuthSwitchRequest",
	AuthMoreData:        "AuthMoreData",
	AuthNextFactor:      "AuthNextFactor",
	HandshakeV10:        "HandshakeV10",
	HandshakeResponse41: "HandshakeResponse41",
}

// Define the map for command packet status
var commandStatusToString = map[byte]string{
	//utility command
	COM_QUIT:             "COM_QUIT",
	COM_INIT_DB:          "COM_INIT_DB",
	COM_FIELD_LIST:       "COM_FIELD_LIST",
	COM_STATISTICS:       "COM_STATISTICS",
	COM_DEBUG:            "COM_DEBUG",
	COM_PING:             "COM_PING",
	COM_CHANGE_USER:      "COM_CHANGE_USER",
	COM_RESET_CONNECTION: "COM_RESET_CONNECTION",
	// COM_SET_OPTION:       "COM_SET_OPTION",
	// command
	COM_QUERY:        "COM_QUERY",
	COM_STMT_PREPARE: "COM_STMT_PREPARE",
	COM_STMT_EXECUTE: "COM_STMT_EXECUTE",
	// COM_STMT_FETCH:          "COM_STMT_FETCH",
	COM_STMT_CLOSE:          "COM_STMT_CLOSE",
	COM_STMT_RESET:          "COM_STMT_RESET",
	COM_STMT_SEND_LONG_DATA: "COM_STMT_SEND_LONG_DATA",
}

// Define the map for cachingSha2Password
var cachingSha2PasswordToString = map[CachingSha2Password]string{
	RequestPublicKey:          "RequestPublicKey",
	FastAuthSuccess:           "FastAuthSuccess",
	PerformFullAuthentication: "PerformFullAuthentication",
}

func StatusToString(status byte) string {
	if str, ok := statusToString[status]; ok {
		return str
	}
	return "UNKNOWN"
}

func AuthStatusToString(status byte) string {
	if str, ok := authStatusToString[status]; ok {
		return str
	}
	return "UNKNOWN"
}

func CommandStatusToString(status byte) string {
	if str, ok := commandStatusToString[status]; ok {
		return str
	}
	return "UNKNOWN"
}

func CachingSha2PasswordToString(status CachingSha2Password) string {
	if str, ok := cachingSha2PasswordToString[status]; ok {
		return str
	}
	return "UNKNOWN"
}
