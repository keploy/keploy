package models

import (
	"github.com/fatih/color"
	"github.com/k0kubun/pp/v3"
)

const (
	NoSqlDB        string = "NO_SQL_DB"
	SqlDB          string = "SQL_DB"
	GRPC           string = "GRPC"
	HttpClient     string = "HTTP_CLIENT"
	TestSetPattern string = "test-set-"
	String         string = "string"
)

var (
	PassThroughHosts = []string{"dc.services.visualstudio.com"}
)

var orangeColorSGR = []color.Attribute{38, 5, 208}

var HighlightString = color.New(orangeColorSGR...).SprintFunc()
var HighlightPassingString = color.New(color.FgGreen).SprintFunc()
var HighlightFailingString = color.New(color.FgRed).SprintFunc()
var GrayString = color.New(color.FgHiBlack)

var PassingColorScheme = pp.ColorScheme{
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

var FailingColorScheme = pp.ColorScheme{
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

const (
	HeaderSize         = 1024
	OKPacketResulSet   = 0x00
	EOFPacketResultSet = 0xfe
	LengthEncodedInt   = 0xfb
)

// ColumnValue represents a value from a column in a result set

const (
	OK               = 0x00
	ERR              = 0xff
	LocalInFile      = 0xfb
	EOF         byte = 0xfe
	flagUnsigned
	statusMoreResultsExists
)

const (
	AuthMoreData                                 byte = 0x01
	CachingSha2PasswordRequestPublicKey               = 2
	CachingSha2PasswordFastAuthSuccess                = 3
	CachingSha2PasswordPerformFullAuthentication      = 4
)

const (
	MaxPacketSize = 1<<24 - 1
)

type CapabilityFlags uint32

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

var mySQLfieldTypeNames = map[byte]string{
	0x00: "MYSQL_TYPE_DECIMAL",
	0x01: "MYSQL_TYPE_TINY",
	0x02: "MYSQL_TYPE_SHORT",
	0x03: "MYSQL_TYPE_LONG",
	0x04: "MYSQL_TYPE_FLOAT",
	0x05: "MYSQL_TYPE_DOUBLE",
	0x06: "MYSQL_TYPE_NULL",
	0x07: "MYSQL_TYPE_TIMESTAMP",
	0x08: "MYSQL_TYPE_LONGLONG",
	0x09: "MYSQL_TYPE_INT24",
	0x0a: "MYSQL_TYPE_DATE",
	0x0b: "MYSQL_TYPE_TIME",
	0x0c: "MYSQL_TYPE_DATETIME",
	0x0d: "MYSQL_TYPE_YEAR",
	0x0e: "MYSQL_TYPE_NEWDATE",
	0x0f: "MYSQL_TYPE_VARCHAR",
	0x10: "MYSQL_TYPE_BIT",
	0xf6: "MYSQL_TYPE_NEWDECIMAL",
	0xf7: "MYSQL_TYPE_ENUM",
	0xf8: "MYSQL_TYPE_SET",
	0xf9: "MYSQL_TYPE_TINY_BLOB",
	0xfa: "MYSQL_TYPE_MEDIUM_BLOB",
	0xfb: "MYSQL_TYPE_LONG_BLOB",
	0xfc: "MYSQL_TYPE_BLOB",
	0xfd: "MYSQL_TYPE_VAR_STRING",
	0xfe: "MYSQL_TYPE_STRING",
	0xff: "MYSQL_TYPE_GEOMETRY",
}
var columnTypeValues = map[string]byte{
	"MYSQL_TYPE_DECIMAL":     0x00,
	"MYSQL_TYPE_TINY":        0x01,
	"MYSQL_TYPE_SHORT":       0x02,
	"MYSQL_TYPE_LONG":        0x03,
	"MYSQL_TYPE_FLOAT":       0x04,
	"MYSQL_TYPE_DOUBLE":      0x05,
	"MYSQL_TYPE_NULL":        0x06,
	"MYSQL_TYPE_TIMESTAMP":   0x07,
	"MYSQL_TYPE_LONGLONG":    0x08,
	"MYSQL_TYPE_INT24":       0x09,
	"MYSQL_TYPE_DATE":        0x0a,
	"MYSQL_TYPE_TIME":        0x0b,
	"MYSQL_TYPE_DATETIME":    0x0c,
	"MYSQL_TYPE_YEAR":        0x0d,
	"MYSQL_TYPE_NEWDATE":     0x0e,
	"MYSQL_TYPE_VARCHAR":     0x0f,
	"MYSQL_TYPE_BIT":         0x10,
	"MYSQL_TYPE_NEWDECIMAL":  0xf6,
	"MYSQL_TYPE_ENUM":        0xf7,
	"MYSQL_TYPE_SET":         0xf8,
	"MYSQL_TYPE_TINY_BLOB":   0xf9,
	"MYSQL_TYPE_MEDIUM_BLOB": 0xfa,
	"MYSQL_TYPE_LONG_BLOB":   0xfb,
	"MYSQL_TYPE_BLOB":        0xfc,
	"MYSQL_TYPE_VAR_STRING":  0xfd,
	"MYSQL_TYPE_STRING":      0xfe,
	"MYSQL_TYPE_GEOMETRY":    0xff,
}

type FieldType byte

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

var fieldTypeNames = map[FieldType]string{
	FieldTypeDecimal:    "FieldTypeDecimal",
	FieldTypeTiny:       "FieldTypeTiny",
	FieldTypeShort:      "FieldTypeShort",
	FieldTypeLong:       "FieldTypeLong",
	FieldTypeFloat:      "FieldTypeFloat",
	FieldTypeDouble:     "FieldTypeDouble",
	FieldTypeNULL:       "FieldTypeNULL",
	FieldTypeTimestamp:  "FieldTypeTimestamp",
	FieldTypeLongLong:   "FieldTypeLongLong",
	FieldTypeInt24:      "FieldTypeInt24",
	FieldTypeDate:       "FieldTypeDate",
	FieldTypeTime:       "FieldTypeTime",
	FieldTypeDateTime:   "FieldTypeDateTime",
	FieldTypeYear:       "FieldTypeYear",
	FieldTypeNewDate:    "FieldTypeNewDate",
	FieldTypeVarChar:    "FieldTypeVarChar",
	FieldTypeBit:        "FieldTypeBit",
	FieldTypeJSON:       "FieldTypeJSON",
	FieldTypeNewDecimal: "FieldTypeNewDecimal",
	FieldTypeEnum:       "FieldTypeEnum",
	FieldTypeSet:        "FieldTypeSet",
	FieldTypeTinyBLOB:   "FieldTypeTinyBLOB",
	FieldTypeMediumBLOB: "FieldTypeMediumBLOB",
	FieldTypeLongBLOB:   "FieldTypeLongBLOB",
	FieldTypeBLOB:       "FieldTypeBLOB",
	FieldTypeVarString:  "FieldTypeVarString",
	FieldTypeString:     "FieldTypeString",
	FieldTypeGeometry:   "FieldTypeGeometry",
}
