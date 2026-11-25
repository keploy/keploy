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
	// tee: the console keeps every line too (a run log that is not quiet).
	tee bool
}

// RouteToRunLog returns logger with every entry at InfoLevel or above also
// written to file, without colour, and, when quiet, raises the console's
// level to WarnLevel. A keploy agent's output started while it holds goes the
// same way (RunLogWriter): its info and debug lines to file, and, when not
// quiet, to console too, so file holds the same lines either way. undo puts
// the console's level back and stops routing the agent's output; the caller
// closes file after it.
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
	if quiet && was < zapcore.WarnLevel {
		LogCfg.Level.SetLevel(zapcore.WarnLevel)
	}
	runLog.Lock()
	runLog.file, runLog.tee = file, !quiet
	runLog.Unlock()
	return routed, func() {
		LogCfg.Level.SetLevel(was)
		runLog.Lock()
		runLog.file, runLog.tee = nil, false
		runLog.Unlock()
	}
}

// RunLogWriter is where a keploy agent's output goes: console, but while a
// run log holds (RouteToRunLog), a writer that sends the agent's info and
// debug lines to the run log (and, when it is not quiet, to console too) and
// the rest (warnings, errors, anything not a log line) to console. An indented line with no level (a log entry's next
// line, a stack frame) goes where the line before it went; any other line
// with no level (a panic, a fatal error, a goroutine's header) goes to
// console. FlushRunLogWriter writes out a last line with no line break, once
// the agent has exited.
func RunLogWriter(console io.Writer) io.Writer {
	runLog.Lock()
	f, tee := runLog.file, runLog.tee
	runLog.Unlock()
	if f == nil {
		return console
	}
	return &levelSplitter{console: console, file: f, tee: tee}
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
	// tee: a line to file goes to console too.
	tee bool
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
		if err := s.writeLine(s.partial[:i+1]); err != nil {
			return len(p), err
		}
		s.partial = s.partial[i+1:]
	}
	if len(s.partial) == 0 {
		s.partial = nil
	}
	return len(p), nil
}

// writeLine writes one line where it goes (RunLogWriter).
func (s *levelSplitter) writeLine(line []byte) error {
	switch m := logLevel.FindSubmatch(line); {
	case m != nil:
		lvl := string(m[1])
		s.toFile = lvl == "DEBUG" || lvl == "INFO"
	case len(bytes.TrimRight(line, "\r\n")) > 0 && line[0] != ' ' && line[0] != '\t':
		s.toFile = false
	}
	if s.toFile {
		if _, err := s.file.Write(ansiCode.ReplaceAll(line, nil)); err != nil || !s.tee {
			return err
		}
	}
	_, err := s.console.Write(line)
	return err
}

// FlushRunLogWriter writes out what w (a RunLogWriter) holds of a last line
// with no line break: a process that dies mid-line ("fatal error: …") ends
// its output so. A no-op on any other writer.
func FlushRunLogWriter(w io.Writer) error {
	s, ok := w.(*levelSplitter)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.partial) == 0 {
		return nil
	}
	line := append(s.partial, '\n')
	s.partial = nil
	return s.writeLine(line)
}
