// Package log provides utility functions for logging.
package log

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"io"

	"go.uber.org/zap"
	"go.uber.org/zap/buffer"
	"go.uber.org/zap/zapcore"
)

var Emoji = "\U0001F430" + " Keploy:"

var LogCfg zap.Config

// ANSI-friendly console encoder
type ansiConsoleEncoder struct {
	*zapcore.EncoderConfig
	zapcore.Encoder
}

func NewANSIConsoleEncoder(cfg zapcore.EncoderConfig) zapcore.Encoder {
	return ansiConsoleEncoder{
		EncoderConfig: &cfg,
		Encoder:       zapcore.NewConsoleEncoder(cfg),
	}
}

func (e ansiConsoleEncoder) EncodeEntry(ent zapcore.Entry, fields []zapcore.Field) (*buffer.Buffer, error) {
	buf, err := e.Encoder.EncodeEntry(ent, fields)
	if err != nil {
		return nil, err
	}

	// Convert escaped unicode sequences back to raw ANSI codes
	bytes := buf.Bytes()
	bytes = replaceAll(bytes, []byte("\\u001b"), []byte("\u001b"))
	bytes = replaceAll(bytes, []byte("\\u001B"), []byte("\u001b"))

	buf.Reset()
	buf.AppendString(string(bytes))
	return buf, nil
}

// replaceAll replaces all occurrences of old with new in the byte slice
func replaceAll(s, old, new []byte) []byte {
	return bytes.Replace(s, old, new, -1)
}

func (e ansiConsoleEncoder) Clone() zapcore.Encoder {
	return ansiConsoleEncoder{
		EncoderConfig: e.EncoderConfig,
		Encoder:       e.Encoder.Clone(),
	}
}

func New() (*zap.Logger, *os.File, error) {
	// Register the ANSI-friendly encoder
	_ = zap.RegisterEncoder("ansiConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewANSIConsoleEncoder(config), nil
	})

	logFile, err := os.OpenFile("keploy-logs.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0777)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open log file: %v", err)
	}

	err = os.Chmod("keploy-logs.txt", 0777)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to set the log file permission to 777: %v", err)
	}

	writer := zapcore.NewMultiWriteSyncer(zapcore.AddSync(os.Stdout), zapcore.AddSync(logFile))

	LogCfg = zap.NewDevelopmentConfig()
	LogCfg.Encoding = "ansiConsole" // Use our custom encoder

	// Customize the encoder config
	LogCfg.EncoderConfig.EncodeTime = customTimeEncoder
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	LogCfg.EncoderConfig.EncodeDuration = zapcore.StringDurationEncoder
	LogCfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder

	// Disable printing the logger name to keep logs clean
	LogCfg.EncoderConfig.EncodeName = nil

	LogCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	LogCfg.DisableStacktrace = true
	LogCfg.EncoderConfig.EncodeCaller = nil

	// Build the core with our custom encoder
	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		writer,
		LogCfg.Level,
	)

	logger := zap.New(core)
	return logger, logFile, nil
}

func ChangeLogLevel(level zapcore.Level) (*zap.Logger, error) {
	LogCfg.Level = zap.NewAtomicLevelAt(level)
	if level == zap.DebugLevel {
		LogCfg.DisableStacktrace = false
		LogCfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
	}

	// Use our custom encoder when building
	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		zapcore.AddSync(os.Stdout),
		LogCfg.Level,
	)

	logger := zap.New(core)
	return logger, nil
}

func AddMode(mode string) (*zap.Logger, error) {
	LogCfg.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		emoji := "\U0001F430"
		modeStr := fmt.Sprintf("Keploy(%s):", mode)
		enc.AppendString(emoji + " " + modeStr + " " + t.Format(time.RFC3339))
	}

	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		zapcore.AddSync(os.Stdout),
		LogCfg.Level,
	)

	logger := zap.New(core)
	return logger, nil
}

func ChangeColorEncoding() (*zap.Logger, error) {
	// For non-color mode, use the standard console encoder
	LogCfg.Encoding = "console"
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	LogCfg.EncoderConfig.EncodeName = nil

	logger, err := LogCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger, nil
}

// moduleCore wraps a zapcore.Core to filter log entries based on include/exclude patterns.
// When debugMode=false: No debug logs appear at all (include/exclude are ignored)
// When debugMode=true: include/exclude filtering is applied
//   - include=[] and exclude=[]: All debug logs appear
//   - include=[A,B] and exclude=[]: Only A and B appear (whitelist)
//   - include=[] and exclude=[A]: All except A appear (blacklist)
//   - include=[A,B,C] and exclude=[B]: Only A and C appear (exclude filters from include)
//
// Hierarchical matching: "proxy" matches "proxy.http".
type moduleCore struct {
	zapcore.Core
	include   []string
	exclude   []string
	debugMode bool // true = debug enabled (filtering active), false = no debug logs at all
}

func (mc *moduleCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	// Non-debug logs always pass through
	if ent.Level > zapcore.DebugLevel {
		return mc.Core.Check(ent, ce)
	}

	// debug=false: No debug logs at all (include/exclude are ignored)
	if !mc.debugMode {
		return ce // Block all debug logs
	}

	// debug=true: Apply include/exclude filtering
	name := ent.LoggerName

	// Step 1: Check include list (whitelist)
	if len(mc.include) > 0 {
		// Include list is specified - only allow modules in the list
		if !matchesHierarchy(name, mc.include) {
			return ce // Not in include list = blocked
		}
	}
	// If include is empty, all modules pass this step (proceed to exclude check)

	// Step 2: Check exclude list (blacklist - filters from include set or all modules)
	if len(mc.exclude) > 0 && matchesHierarchy(name, mc.exclude) {
		return ce // In exclude list = blocked
	}

	// Passed all filters = allow
	return mc.Core.Check(ent, ce)
}

// matchesHierarchy checks if name matches any pattern hierarchically.
// "proxy" matches "proxy", "proxy.http", "proxy.http.request", etc.
func matchesHierarchy(name string, patterns []string) bool {
	for _, pattern := range patterns {
		if name == pattern || strings.HasPrefix(name, pattern+".") {
			return true
		}
	}
	return false
}

func (mc *moduleCore) With(fields []zapcore.Field) zapcore.Core {
	return &moduleCore{
		Core:      mc.Core.With(fields),
		include:   mc.include,
		exclude:   mc.exclude,
		debugMode: mc.debugMode,
	}
}

var consoleWriter = zapcore.AddSync(os.Stdout)

func SetConsoleWriter(w io.Writer) {
	consoleWriter = zapcore.AddSync(w)
}

func SetDebugModules(include, exclude []string, debugMode bool) (*zap.Logger, error) {
	// Enable Debug level globally in the configuration so the underlying core accepts it.
	LogCfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
	LogCfg.DisableStacktrace = false
	LogCfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
	// Show logger name to identify parsers (e.g., "proxy.mysql", "proxy.postgres")
	LogCfg.EncoderConfig.EncodeName = zapcore.FullNameEncoder

	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	// Using os.Stdout as per ChangeLogLevel implementation
	core := zapcore.NewCore(
		encoder,
		consoleWriter,
		LogCfg.Level,
	)

	wrappedCore := &moduleCore{
		Core:      core,
		include:   include,
		exclude:   exclude,
		debugMode: debugMode,
	}

	logger := zap.New(wrappedCore)
	return logger, nil
}
