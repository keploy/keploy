package models

import (
	"github.com/k0kubun/pp/v3"
	"github.com/fatih/color"
)

const (
	NoSqlDB    string = "NO_SQL_DB"
	SqlDB      string = "SQL_DB"
	GRPC       string = "GRPC"
	HttpClient string = "HTTP_CLIENT"
	TestSetPattern string = "test-set-"
)

var HighlightString = color.New(color.FgYellow).SprintFunc()
var HighlightPassingString = color.New(color.FgGreen).SprintFunc()
var HighlightFailingString = color.New(color.FgRed).SprintFunc()

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