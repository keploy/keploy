// Package vitesslog keeps the vitess libraries keploy parses MySQL with from
// writing a statement, or any other text they log, to disk or to stderr.
//
// vitess logs through its own log package (vitess.io/vitess/go/vt/log), whose
// functions are package variables bound to glog. Nothing in keploy configures
// glog, and glog then writes every line to files under os.TempDir()
// (<program>.<host>.<user>.log.<SEVERITY>.<time>.<pid>), and ERROR and higher
// to stderr as well. The lines keploy's parse paths reach carry customer
// content: sqlparser's Parse2 logs "ignoring error parsing DDL '%s'" with the
// whole statement whenever it only partly parses a DDL, and that runs on
// every replay of such a recorded query.
//
// This package's init rebinds every one of those variables, so no vitess line
// ever reaches glog. A line goes to keploy's logger (SetLogger) at debug
// level instead, with its text withheld: it names only the vitess severity
// and the vitess source line that logged it, never its format or arguments.
// Exit and Fatal still end the process as glog's do (codes 1 and 2), since
// their callers do not expect them to return.
//
// Every keploy package that imports a vitess package imports this one, so the
// rebinding has run before any vitess code can log (Go runs an imported
// package's init first). TestEveryVitessImporterImportsVitesslog holds that.
package vitesslog

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"

	"github.com/golang/glog"
	"go.uber.org/zap"
	vtlog "vitess.io/vitess/go/vt/log"
)

// The message of the line a withheld vitess line becomes.
const withheldMsg = "a vitess log line was withheld: its text can hold a SQL statement or its values"

var sink atomic.Pointer[zap.Logger]

// SetLogger sets the logger withheld vitess lines are noted on. Until it is
// called they are dropped. A nil logger is ignored.
func SetLogger(logger *zap.Logger) {
	if logger != nil {
		sink.Store(logger)
	}
}

// exit is os.Exit, a variable so a test can see Exit and Fatal end the
// process without ending the test.
var exit = os.Exit

func init() {
	// V gates vitess's verbose lines, which glog's Verbose methods write
	// straight to its files: none is enabled.
	vtlog.V = func(glog.Level) glog.Verbose { return false }

	vtlog.Info = func(...any) { withhold("INFO", 0) }
	vtlog.Infof = func(string, ...any) { withhold("INFO", 0) }
	vtlog.InfoDepth = func(depth int, _ ...any) { withhold("INFO", depth) }

	vtlog.Warning = func(...any) { withhold("WARNING", 0) }
	vtlog.Warningf = func(string, ...any) { withhold("WARNING", 0) }
	vtlog.WarningDepth = func(depth int, _ ...any) { withhold("WARNING", depth) }

	vtlog.Error = func(...any) { withhold("ERROR", 0) }
	vtlog.Errorf = func(string, ...any) { withhold("ERROR", 0) }
	vtlog.ErrorDepth = func(depth int, _ ...any) { withhold("ERROR", depth) }

	vtlog.Exit = func(...any) { withholdAndExit("EXIT", 0, 1) }
	vtlog.Exitf = func(string, ...any) { withholdAndExit("EXIT", 0, 1) }
	vtlog.ExitDepth = func(depth int, _ ...any) { withholdAndExit("EXIT", depth, 1) }

	vtlog.Fatal = func(...any) { withholdAndExit("FATAL", 0, 2) }
	vtlog.Fatalf = func(string, ...any) { withholdAndExit("FATAL", 0, 2) }
	vtlog.FatalDepth = func(depth int, _ ...any) { withholdAndExit("FATAL", depth, 2) }
}

// withhold notes one vitess line on the logger, without its text. depth is
// the extra frames a *Depth call asks to skip past its caller.
func withhold(severity string, depth int) {
	logger := sink.Load()
	if logger == nil {
		return
	}
	logger.Debug(withheldMsg,
		zap.String("vitessSeverity", severity),
		zap.String("vitessSource", source(depth+3)))
}

func withholdAndExit(severity string, depth, code int) {
	logger := sink.Load()
	if logger != nil {
		logger.Error(withheldMsg,
			zap.String("vitessSeverity", severity),
			zap.String("vitessSource", source(depth+3)),
			zap.Int("exitCode", code))
		_ = logger.Sync()
	}
	exit(code)
}

// source is the vitess line skip frames up, as <dir>/<file>:<line> (for
// example sqlparser/parser.go:84): the library's source location, which
// holds no customer content.
func source(skip int) string {
	_, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	return filepath.Base(filepath.Dir(file)) + "/" + filepath.Base(file) + ":" + strconv.Itoa(line)
}
