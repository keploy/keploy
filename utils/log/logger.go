package log

import (
	"fmt"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var Emoji = "\U0001F430" + " Keploy:"

// TODO find better way than global variable

var LogCfg zap.Config

func New() (*zap.Logger, *os.File, error) {
	_ = zap.RegisterEncoder("colorConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewColor(config, true), nil
	})
	_ = zap.RegisterEncoder("nonColorConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewColor(config, false), nil
	})

	logFile, err := os.OpenFile("keploy-logs.txt", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0777)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open log file: %v", err)
	}

	writer := zapcore.NewMultiWriteSyncer(zapcore.AddSync(os.Stdout), zapcore.AddSync(logFile))

	LogCfg = zap.NewDevelopmentConfig()

	LogCfg.Encoding = "colorConsole"

	// Customize the encoder config to put the emoji at the beginning.
	LogCfg.EncoderConfig.EncodeTime = customTimeEncoder
	LogCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder

	LogCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	LogCfg.DisableStacktrace = true
	LogCfg.EncoderConfig.EncodeCaller = nil

	core := zapcore.NewCore(
		zapcore.NewConsoleEncoder(LogCfg.EncoderConfig),
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

	logger, err := LogCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger, nil
}

func AddMode(mode string) (*zap.Logger, error) {
	// Get the current logger configuration
	cfg := LogCfg
	// Update the time encoder with the new values
	cfg.EncoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		emoji := "\U0001F430"
		mode := fmt.Sprintf("Keploy(%s):", mode)
		enc.AppendString(emoji + " " + mode + " " + t.Format(time.RFC3339) + " ")
	}
	// Rebuild the logger with the updated configuration
	newLogger, err := cfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to add mode to logger: %v", err)
	}
	return newLogger, nil
}

func ChangeColorEncoding() (*zap.Logger, error) {
	LogCfg.Encoding = "nonColorConsole"
	logger, err := LogCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger, nil
}
