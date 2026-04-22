package models

import (
	"context"
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

// WindowSnapshot is a coherent point-in-time read of the two test-window
// state bits MockManager exposes:
//
//   - Active:         at least one real test window is currently set
//                     (SetCurrentTestWindow / SetMocksWithWindow with
//                     non-zero start+end; BaseTime staging does NOT
//                     activate — see mockmanager.go isInitialStaging).
//   - FirstTestFired: at least one real (non-BaseTime) test window has
//                     ever been set; sticky once true.
//
// The principal-engineer review flagged a torn-read hazard: the legacy
// IsTestWindowActive / HasFirstTestFired accessors read under different
// locks (windowMu vs swapMu respectively), so a caller that observes
// both bits sequentially during a SetCurrentTestWindow /
// SetMocksWithWindow transition can see the forbidden intermediate
// state Active=true && FirstTestFired=false (window flipped on, but
// firstWindowStart not yet published). Cross-tier misroute follows:
// the v3 dispatcher's routeTransactional picks PerTest, but TierIndex's
// orderForCurrentState may fall to a different branch on the next
// statement in the same bundle.
//
// Callers that need the PAIR as a coherent snapshot (v3 dispatcher's
// routeTransactional, v3 types.TierIndex.orderForCurrentState) MUST use
// the single MockManager.WindowSnapshot accessor, which takes one outer
// lock. The individual bool accessors remain for legacy callers that
// only read one bit at a time.
type WindowSnapshot struct {
	Active         bool
	FirstTestFired bool
}

var IsAnsiDisabled = false

var HighlightString = func(a ...interface{}) string {
	if IsAnsiDisabled {
		return fmt.Sprint(a...)
	}
	return color.New(orangeColorSGR...).SprintFunc()(a...)
}

var HighlightPassingString = func(a ...interface{}) string {
	if IsAnsiDisabled {
		return fmt.Sprint(a...)
	}
	return color.New(color.FgGreen).SprintFunc()(a...)
}

var HighlightFailingString = func(a ...interface{}) string {
	if IsAnsiDisabled {
		return fmt.Sprint(a...)
	}
	return color.New(color.FgRed).SprintFunc()(a...)
}

var HighlightGrayString = func(a ...interface{}) string {
	if IsAnsiDisabled {
		return fmt.Sprint(a...)
	}
	return color.New(color.FgHiBlack).SprintFunc()(a...)
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
const PostTLSModeKey contextKey = "postTLSMode"
const TLSHandshakeStoreKey contextKey = "tlsHandshakeStore"

// CapturedReqTimeKey and CapturedRespTimeKey each hold a func() time.Time
// that returns the wall-clock time associated with the most recent
// REQUEST or RESPONSE chunk seen on the enclosing connection, as
// observed by the capture layer. Integration parsers (mongo/mysql/pg/
// redis) should call CapturedReqTime(ctx) when stamping a mock's
// reqTimestampMock and CapturedRespTime(ctx) for the response side so
// timestamps reflect true packet arrival order rather than
// parser-goroutine scheduling order. A single "latest event" function
// is unsafe: the response-side fallback would clobber a recorded
// reqTimestampMock when a stale response from a previous exchange is
// still in the per-connection atomic at the time the next request is
// read.
//
// If no source is installed on the context (default proxy path, unit
// tests, module init) the helpers fall back to time.Now() so call
// sites can use them unconditionally.
const (
	CapturedReqTimeKey  contextKey = "capturedReqTime"
	CapturedRespTimeKey contextKey = "capturedRespTime"
)

// CapturedReqTime returns the wall time associated with the most
// recent request chunk on this connection, or time.Now() if the
// context doesn't carry a captured-time source.
func CapturedReqTime(ctx context.Context) time.Time {
	return capturedTime(ctx, CapturedReqTimeKey)
}

// CapturedRespTime returns the wall time associated with the most
// recent response chunk on this connection, or time.Now() if the
// context doesn't carry a captured-time source.
func CapturedRespTime(ctx context.Context) time.Time {
	return capturedTime(ctx, CapturedRespTimeKey)
}

func capturedTime(ctx context.Context, key contextKey) time.Time {
	if ctx == nil {
		return time.Now()
	}
	if v := ctx.Value(key); v != nil {
		if fn, ok := v.(func() time.Time); ok {
			if t := fn(); !t.IsZero() {
				return t
			}
		}
	}
	return time.Now()
}
