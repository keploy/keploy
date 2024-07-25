package models

import (
	"fmt"
	"time"

	"github.com/fatih/color"
	"github.com/k0kubun/pp/v3"
	"go.keploy.io/server/v2/config"
)

// Patterns for different usecases in keploy
const (
	NoSQLDB             string = "NO_SQL_DB"
	SQLDB               string = "SQL_DB"
	GRPC                string = "GRPC"
	HTTPClient          string = "HTTP_CLIENT"
	TestSetPattern      string = "test-set-"
	String              string = "string"
	TestRunTemplateName string = "test-run-"
)

const (
	Unknown    config.Language = "Unknown"    // Unknown language
	Go         config.Language = "go"         // Go language
	Java       config.Language = "java"       // Java language
	Python     config.Language = "python"     // Python language
	Javascript config.Language = "javascript" // Javascript language
)

var (
	PassThroughHosts = []string{"^dc\\.services\\.visualstudio\\.com$"}
)

var orangeColorSGR = []color.Attribute{38, 5, 208}

var BaseTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var IsAnsiDisabled = false

var HighlightString = func(a ...interface{}) string {
	if IsAnsiDisabled {
		return fmt.Sprint(a)
	}
	return color.New(orangeColorSGR...).SprintFunc()(a)
}

var HighlightPassingString = func(a ...interface{}) string {
	if IsAnsiDisabled {
		return fmt.Sprint(a)
	}
	return color.New(color.FgGreen).SprintFunc()(a)
}

var HighlightFailingString = func(a ...interface{}) string {
	if IsAnsiDisabled {
		return fmt.Sprint(a)
	}
	return color.New(color.FgRed).SprintFunc()(a)
}

var HighlightGrayString = func(a ...interface{}) string {
	if IsAnsiDisabled {
		return fmt.Sprint(a)
	}
	return color.New(color.FgHiBlack).SprintFunc()(a)
}

var defaultColorScheme = pp.ColorScheme{
	Bool:            pp.NoColor,
	Integer:         pp.NoColor,
	Float:           pp.NoColor,
	String:          pp.NoColor,
	StringQuotation: pp.NoColor,
	EscapedChar:     pp.NoColor,
	FieldName:       pp.NoColor,
	PointerAdress:   pp.NoColor,
	Nil:             pp.NoColor,
	Time:            pp.NoColor,
	StructName:      pp.NoColor,
	ObjectLength:    pp.NoColor,
}

var GetPassingColorScheme = func() pp.ColorScheme {
	if IsAnsiDisabled {
		return defaultColorScheme
	}
	return pp.ColorScheme{
		String:          pp.Green,
		StringQuotation: pp.Green | pp.Bold,
		FieldName:       pp.White,
		Integer:         pp.Blue | pp.Bold,
		StructName:      pp.NoColor,
		Bool:            pp.Cyan | pp.Bold,
		Float:           pp.Magenta | pp.Bold,
		EscapedChar:     pp.Magenta | pp.Bold,
		PointerAdress:   pp.Blue | pp.Bold,
		Nil:             pp.Cyan | pp.Bold,
		Time:            pp.Blue | pp.Bold,
		ObjectLength:    pp.Blue,
	}
}

var GetFailingColorScheme = func() pp.ColorScheme {
	if IsAnsiDisabled {
		return defaultColorScheme
	}
	return pp.ColorScheme{
		Bool:            pp.Cyan | pp.Bold,
		Integer:         pp.Blue | pp.Bold,
		Float:           pp.Magenta | pp.Bold,
		String:          pp.Red,
		StringQuotation: pp.Red | pp.Bold,
		EscapedChar:     pp.Magenta | pp.Bold,
		FieldName:       pp.Yellow,
		PointerAdress:   pp.Blue | pp.Bold,
		Nil:             pp.Cyan | pp.Bold,
		Time:            pp.Blue | pp.Bold,
		StructName:      pp.White,
		ObjectLength:    pp.Blue,
	}
}

// MySQL constants
const (
	TypeDecimal    byte = 0x00
	TypeTiny       byte = 0x01
	TypeShort      byte = 0x02
	TypeLong       byte = 0x03
	TypeFloat      byte = 0x04
	TypeDouble     byte = 0x05
	TypeNull       byte = 0x06
	TypeTimestamp  byte = 0x07
	TypeLongLong   byte = 0x08
	TypeInt24      byte = 0x09
	TypeDate       byte = 0x0a
	TypeTime       byte = 0x0b
	TypeDateTime   byte = 0x0c
	TypeYear       byte = 0x0d
	TypeNewDate    byte = 0x0e
	TypeVarChar    byte = 0x0f
	TypeBit        byte = 0x10
	TypeNewDecimal byte = 0xf6
	TypeEnum       byte = 0xf7
	TypeSet        byte = 0xf8
	TypeTinyBlob   byte = 0xf9
	TypeMediumBlob byte = 0xfa
	TypeLongBlob   byte = 0xfb
	TypeBlob       byte = 0xfc
	TypeVarString  byte = 0xfd
	TypeString     byte = 0xfe
	TypeGeometry   byte = 0xff
)

// MySQL constants
const (
	HeaderSize         = 1024
	OKPacketResulSet   = 0x00
	EOFPacketResultSet = 0xfe
	LengthEncodedInt   = 0xfb
)

// MySQL constants
const (
	OK               = 0x00
	ERR              = 0xff
	LocalInFile      = 0xfb
	EOF         byte = 0xfe
)

// MySQL constants
const (
	AuthMoreData                                 byte = 0x01
	CachingSha2PasswordRequestPublicKey          byte = 2
	CachingSha2PasswordFastAuthSuccess           byte = 3
	CachingSha2PasswordPerformFullAuthentication byte = 4
)

// MySQL constants
const (
	MaxPacketSize = 1<<24 - 1
)

type CapabilityFlags uint32

// MySQL constants
const (
	CLIENT_LONG_PASSWORD CapabilityFlags = 1 << iota
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
	CLIENT_SSL = 0x00000800
	CLIENT_IGNORE_SIGPIPE
	CLIENT_TRANSACTIONS
	CLIENT_RESERVED
	CLIENT_SECURE_CONNECTION
	CLIENT_MULTI_STATEMENTS = 1 << (iota + 2)
	CLIENT_MULTI_RESULTS
	CLIENT_PS_MULTI_RESULTS
	CLIENT_PLUGIN_AUTH
	CLIENT_CONNECT_ATTRS
	CLIENT_PLUGIN_AUTH_LENENC_CLIENT_DATA
	CLIENT_CAN_HANDLE_EXPIRED_PASSWORDS
	CLIENT_SESSION_TRACK
	CLIENT_DEPRECATE_EOF
)

type FieldType byte

// MySQL constants
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

// MySQL constants
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

// MySQL constants
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

type contextKey string

const ErrGroupKey contextKey = "errGroup"
const ClientConnectionIDKey contextKey = "clientConnectionId"
const DestConnectionIDKey contextKey = "destConnectionId"
