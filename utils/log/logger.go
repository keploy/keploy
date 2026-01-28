// Package log provides utility functions for logging.
package log

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
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

var defaultWriter zapcore.WriteSyncer

func New() (*zap.Logger, *os.File, error) {
	// Register the ANSI-friendly encoder
	_ = zap.RegisterEncoder("ansiConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewANSIConsoleEncoder(config), nil
	})

	logPath := filepath.Join(os.TempDir(), "keploy-logs.txt")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open log file: %v", err)
	}

	// We don't necessarily need to chmod 777 in temp dir, typically 666 or default is fine,
	// but user asked for temp dir. Keep it simple.
	// Also removed the explicit chmod 777 as it might be insecure and unnecessary in temp.
	// If it was for docker permissions, temp dir usually handles this better or 0666 is enough.

	defaultWriter = zapcore.NewMultiWriteSyncer(zapcore.AddSync(os.Stdout), zapcore.AddSync(logFile))

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
		defaultWriter,
		LogCfg.Level,
	)

	logger := zap.New(core)
	logger.Debug("Log file created", zap.String("path", logPath))
	return logger, logFile, nil
}

func ChangeLogLevel(level zapcore.Level) (*zap.Logger, error) {
	// Update the existing atomic level so all cores using it get updated
	LogCfg.Level.SetLevel(level)

	if level == zap.DebugLevel {
		LogCfg.DisableStacktrace = false
		LogCfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
	} else {
		LogCfg.DisableStacktrace = true
		LogCfg.EncoderConfig.EncodeCaller = nil
	}

	// Use our custom encoder when building
	encoder := NewANSIConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		defaultWriter,
		LogCfg.Level,
	)

	logger := zap.New(core)
	return logger, nil
}

func AddMode(mode string) (*zap.Logger, error) {
	// We clone the config for AddMode to avoid affecting other loggers' time formatting,
	// but we keep the SAME AtomicLevel to ensure level updates still propagate.
	cfg := LogCfg
	cfg.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		emoji := "\U0001F430"
		modeStr := fmt.Sprintf("Keploy(%s):", mode)
		enc.AppendString(emoji + " " + modeStr + " " + t.Format(time.RFC3339))
	}

	encoder := NewANSIConsoleEncoder(cfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		defaultWriter,
		LogCfg.Level, // Use shared level
	)

	logger := zap.New(core)
	return logger, nil
}

func ChangeColorEncoding() (*zap.Logger, error) {
	// For non-color mode, use the standard console encoder
	LogCfg.Encoding = "console"
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

	encoder := zapcore.NewConsoleEncoder(LogCfg.EncoderConfig)
	core := zapcore.NewCore(
		encoder,
		defaultWriter,
		LogCfg.Level, // Use shared level
	)

	logger := zap.New(core)
	return logger, nil
}
