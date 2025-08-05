package log

import (
	"fmt"
	"log"
	"os"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var Emoji = "\U0001F430" + " Keploy:"

// LoggerConfig holds the configuration for the logger
type LoggerConfig struct {
	config zap.Config
}

// NewLoggerConfig creates a new logger configuration with default settings
func NewLoggerConfig() *LoggerConfig {
	_ = zap.RegisterEncoder("colorConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewColor(config, true), nil
	})
	_ = zap.RegisterEncoder("nonColorConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewColor(config, false), nil
	})

	cfg := zap.NewDevelopmentConfig()
	cfg.Encoding = "colorConsole"

	// Customize the encoder config to put the emoji at the beginning.
	cfg.EncoderConfig.EncodeTime = customTimeEncoder
	cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder

	cfg.OutputPaths = []string{
		"stdout",
		"./keploy-logs.txt",
	}

	cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	cfg.DisableStacktrace = true
	cfg.EncoderConfig.EncodeCaller = nil

	return &LoggerConfig{config: cfg}
}

// ensureLogFile ensures the log file exists with proper permissions
func (lc *LoggerConfig) ensureLogFile() error {
	// Check if keploy-log.txt exists, if not create it.
	_, err := os.Stat("keploy-logs.txt")
	if os.IsNotExist(err) {
		_, err := os.Create("keploy-logs.txt")
		if err != nil {
			return fmt.Errorf("failed to create the log file: %v", err)
		}
	}

	// Check if the permission of the log file is 777, if not set it to 777.
	fileInfo, err := os.Stat("keploy-logs.txt")
	if err != nil {
		log.Println(Emoji, "failed to get the log file info", err)
		return fmt.Errorf("failed to get the log file info: %v", err)
	}
	if fileInfo.Mode().Perm() != 0777 {
		// Set the permissions of the log file to 777.
		err = os.Chmod("keploy-logs.txt", 0777)
		if err != nil {
			log.Println(Emoji, "failed to set the log file permission to 777", err)
			return fmt.Errorf("failed to set the log file permission to 777: %v", err)
		}
	}
	return nil
}

// Build creates a new logger instance from the configuration
func (lc *LoggerConfig) Build() (*zap.Logger, error) {
	if err := lc.ensureLogFile(); err != nil {
		return nil, err
	}

	logger, err := lc.config.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger, nil
}

// SetLevel changes the log level of the configuration
func (lc *LoggerConfig) SetLevel(level zapcore.Level) {
	lc.config.Level = zap.NewAtomicLevelAt(level)
	if level == zap.DebugLevel {
		lc.config.DisableStacktrace = false
		lc.config.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
	}
}

// SetEncoding changes the encoding of the configuration
func (lc *LoggerConfig) SetEncoding(encoding string) {
	lc.config.Encoding = encoding
}

// SetTimeEncoder sets a custom time encoder for the configuration
func (lc *LoggerConfig) SetTimeEncoder(encoder func(time.Time, zapcore.PrimitiveArrayEncoder)) {
	lc.config.EncoderConfig.EncodeTime = encoder
}

// New creates a new logger with default configuration
func New() (*zap.Logger, error) {
	config := NewLoggerConfig()
	return config.Build()
}

// ChangeLogLevel creates a new logger with the specified log level
func ChangeLogLevel(level zapcore.Level) (*zap.Logger, error) {
	config := NewLoggerConfig()
	config.SetLevel(level)
	return config.Build()
}

// AddMode creates a new logger with mode information in the time encoder
func AddMode(mode string) (*zap.Logger, error) {
	config := NewLoggerConfig()
	// Update the time encoder with the new values
	config.SetTimeEncoder(func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		emoji := "\U0001F430"
		mode := fmt.Sprintf("Keploy(%s):", mode)
		enc.AppendString(emoji + " " + mode + " " + t.Format(time.RFC3339) + " ")
	})
	// Rebuild the logger with the updated configuration
	return config.Build()
}

// ChangeColorEncoding creates a new logger with non-color console encoding
func ChangeColorEncoding() (*zap.Logger, error) {
	config := NewLoggerConfig()
	config.SetEncoding("nonColorConsole")
	return config.Build()
}
