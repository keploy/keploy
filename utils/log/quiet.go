package log

import (
	"os"
	"sync/atomic"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	Show = zap.Field{Key: "keploy-show", Type: zapcore.SkipType}
	Hide = zap.Field{Key: "keploy-hide", Type: zapcore.SkipType}
)

const factPrefix = "keploy: "

var (
	quiet   atomic.Bool
	runFile atomic.Pointer[os.File]
)

func Quiet() (*zap.Logger, error) {
	ensureLogCfg()
	quiet.Store(runFile.Load() != nil)
	return reattachDebugFileSink(zap.New(newRedactingCore(primaryCore(LogCfg, LogCfg.Level)))), nil
}

func Quieted() bool { return quiet.Load() }

func SetRunFile(f *os.File) { runFile.Store(f) }

func Facts(logger *zap.Logger) *zap.Logger {
	ensureLogCfg()
	shown := newRedactingCore(factCore{Core: newFactConsole(LogCfg.Level), min: zapcore.InvalidLevel})
	return logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		return zapcore.NewTee(unshown{c}, shown)
	}))
}

func primaryCore(cfg zap.Config, level zapcore.LevelEnabler) zapcore.Core {
	if !quiet.Load() {
		return consoleCore(cfg, primarySyncer(), level)
	}
	core := zapcore.Core(factCore{Core: newFactConsole(level), min: zapcore.WarnLevel})
	if f := runFile.Load(); f != nil {
		core = zapcore.NewTee(core, consoleCore(cfg, zapcore.AddSync(f), level))
	}
	return core
}

func newFactConsole(level zapcore.LevelEnabler) zapcore.Core {
	enc := newPlainConsoleEncoder(zapcore.EncoderConfig{MessageKey: "msg", ConsoleSeparator: " "})
	return zapcore.NewCore(enc, wrapWriter(primarySyncer(), enc), level)
}

func marked(fields []zapcore.Field, mark zap.Field) bool {
	for _, f := range fields {
		if f.Key == mark.Key && f.Type == zapcore.SkipType {
			return true
		}
	}
	return false
}

type factCore struct {
	zapcore.Core
	min zapcore.Level
}

func (c factCore) With(fields []zapcore.Field) zapcore.Core {
	return factCore{Core: c.Core.With(fields), min: c.min}
}

func (c factCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c factCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	if marked(fields, Hide) || ent.Level < c.min && !marked(fields, Show) {
		return nil
	}
	ent.Message = factPrefix + ent.Message
	next := ""
	for _, f := range fields {
		switch {
		case f.Type == zapcore.ErrorType && f.Key == "error":
			if err, ok := f.Interface.(error); ok && err != nil {
				ent.Message += ": " + err.Error()
			}
		case f.Type == zapcore.StringType && f.Key == "next_step":
			next = f.String
		}
	}
	if err := c.Core.Write(ent, nil); err != nil || next == "" {
		return err
	}
	ent.Message = factPrefix + "next step: " + next
	return c.Core.Write(ent, nil)
}

type unshown struct{ zapcore.Core }

func (c unshown) With(fields []zapcore.Field) zapcore.Core { return unshown{c.Core.With(fields)} }

func (c unshown) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

func (c unshown) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	if marked(fields, Show) {
		return nil
	}
	return c.Core.Write(ent, fields)
}
