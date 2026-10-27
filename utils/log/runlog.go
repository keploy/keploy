package log

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"sync"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// The run log. Under keploy mock replay the lines keploy logs about itself
// (starting its agent and proxy, loading the recording, stopping) go to the
// run's own file, keploy.log, and the console keeps what is for its reader:
// warnings and errors, and what keploy prints outside its log (its logo and
// version, the test command's output, the run's end block).

var runLog struct {
	sync.Mutex
	file *os.File
}

// RouteToRunLog returns logger with every entry at InfoLevel or above also
// written to file, without colour, and, when quiet, raises the console's
// level to WarnLevel, and splits a keploy agent's output started while it
// holds the same way (RunLogWriter). undo puts the console's level back and
// stops routing the agent's output; the caller closes file after it.
func RouteToRunLog(logger *zap.Logger, file *os.File, quiet bool) (routed *zap.Logger, undo func()) {
	ensureLogCfg()
	cfg := LogCfg
	cfg.Encoding = "console"
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	fileCore := newRedactingCore(consoleCore(cfg, zapcore.AddSync(file), zapcore.InfoLevel))
	routed = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(c, fileCore)
	}))
	was := LogCfg.Level.Level()
	if !quiet {
		return routed, func() {}
	}
	if was < zapcore.WarnLevel {
		LogCfg.Level.SetLevel(zapcore.WarnLevel)
	}
	runLog.Lock()
	runLog.file = file
	runLog.Unlock()
	return routed, func() {
		LogCfg.Level.SetLevel(was)
		runLog.Lock()
		runLog.file = nil
		runLog.Unlock()
	}
}

// RunLogWriter is where a keploy agent's output goes: console, but while a
// run log holds (RouteToRunLog), a writer that sends the agent's info and
// debug lines to the run log and the rest (warnings, errors, anything not a
// log line) to console. A line with no level (a log entry's next line) goes
// where the line before it went.
func RunLogWriter(console io.Writer) io.Writer {
	runLog.Lock()
	f := runLog.file
	runLog.Unlock()
	if f == nil {
		return console
	}
	return &levelSplitter{console: console, file: f}
}

// ansiCode is a terminal colour code, which the run log is written without.
var ansiCode = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// logLevel finds a log line's level as keploy's console encoder writes it:
// tab-separated after the time, coloured or not.
var logLevel = regexp.MustCompile(`\t(?:\x1b\[[0-9;]*m)?(DEBUG|INFO|WARN|ERROR|DPANIC|PANIC|FATAL)(?:\x1b\[[0-9;]*m)?\t`)

// levelSplitter splits an agent's output by line (levelSplitter.Write).
type levelSplitter struct {
	mu      sync.Mutex
	console io.Writer
	file    io.Writer
	partial []byte
	toFile  bool
}

func (s *levelSplitter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		line := s.partial[:i+1]
		if m := logLevel.FindSubmatch(line); m != nil {
			lvl := string(m[1])
			s.toFile = lvl == "DEBUG" || lvl == "INFO"
		}
		var err error
		if s.toFile {
			_, err = s.file.Write(ansiCode.ReplaceAll(line, nil))
		} else {
			_, err = s.console.Write(line)
		}
		if err != nil {
			return len(p), err
		}
		s.partial = s.partial[i+1:]
	}
	if len(s.partial) == 0 {
		s.partial = nil
	}
	return len(p), nil
}
