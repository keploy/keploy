package models

import (
	"errors"
	"fmt"
	"time"

	"github.com/fatih/color"
	"github.com/k0kubun/pp/v3"
)

type Language string

// String is used both by fmt.Print and by Cobra in help text
func (e *Language) String() string {
	return string(*e)
}

// Set must have pointer receiver so it doesn't change the value of a copy
func (e *Language) Set(v string) error {
	switch v {
	case "go", "java", "python", "javascript":
		*e = Language(v)
		return nil
	default:
		return errors.New(`must be one of "go", "java", "python" or "javascript"`)
	}
}

// Type is only used in help text
func (e *Language) Type() string {
	return "myEnum"
}

// Patterns for different usecases in keploy
const (
	NoSQLDB             string = "NO_SQL_DB"
	SQLDB               string = "SQL_DB"
	GRPC                string = "GRPC"
	HTTPClient          string = "HTTP_CLIENT"
	HTTP2Client         string = "HTTP2_CLIENT"
	TestSetPattern      string = "test-set-"
	String              string = "string"
	TestRunTemplateName string = "test-run-"
)

const (
	DefaultIncomingProxyPort uint16 = 36789
)

const (
	Unknown    Language = "Unknown"    // Unknown language
	Go         Language = "go"         // Go language
	Java       Language = "java"       // Java language
	Python     Language = "python"     // Python language
	Javascript Language = "javascript" // Javascript language
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

type contextKey string

const ErrGroupKey contextKey = "errGroup"
const ClientConnectionIDKey contextKey = "clientConnectionId"
const DestConnectionIDKey contextKey = "destConnectionId"
