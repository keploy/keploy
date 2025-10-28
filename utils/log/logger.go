// Package log provides utility functions for logging.
package log

import (
	"bytes"
	"fmt"
	"os"
	"time"

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
	cfg := LogCfg
	cfg.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		emoji := "\U0001F430"
		modeStr := fmt.Sprintf("Keploy(%s):", mode)
		enc.AppendString(emoji + " " + modeStr + " " + t.Format(time.RFC3339))
	}

	encoder := NewANSIConsoleEncoder(cfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		zapcore.AddSync(os.Stdout),
		cfg.Level,
	)

	logger := zap.New(core)
	return logger, nil
}

func ChangeColorEncoding() (*zap.Logger, error) {
	// For non-color mode, use the standard console encoder
	LogCfg.Encoding = "console"
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

	logger, err := LogCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger, nil
}
