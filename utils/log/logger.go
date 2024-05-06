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

// TODO find better way than global variable
var logCfg zap.Config

func New() (*zap.Logger, error) {
	_ = zap.RegisterEncoder("colorConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewColor(config, true), nil
	})
	_ = zap.RegisterEncoder("nonColorConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewColor(config, false), nil
	})

	logCfg = zap.NewDevelopmentConfig()

	logCfg.Encoding = "colorConsole"

	// Customize the encoder config to put the emoji at the beginning.
	logCfg.EncoderConfig.EncodeTime = customTimeEncoder
	logCfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder

	logCfg.OutputPaths = []string{
		"stdout",
		"./keploy-logs.txt",
	}

	// Check if keploy-log.txt exists, if not create it.
	_, err := os.Stat("keploy-logs.txt")
	if os.IsNotExist(err) {
		_, err := os.Create("keploy-logs.txt")
		if err != nil {
			return nil, fmt.Errorf("failed to create the log file: %v", err)
		}
	}

	// Check if the permission of the log file is 777, if not set it to 777.
	fileInfo, err := os.Stat("keploy-logs.txt")
	if err != nil {
		log.Println(Emoji, "failed to get the log file info", err)
		return nil, fmt.Errorf("failed to get the log file info: %v", err)
	}
	if fileInfo.Mode().Perm() != 0777 {
		// Set the permissions of the log file to 777.
		err = os.Chmod("keploy-logs.txt", 0777)
		if err != nil {
			log.Println(Emoji, "failed to set the log file permission to 777", err)
			return nil, fmt.Errorf("failed to set the log file permission to 777: %v", err)
		}
	}

	logCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	logCfg.DisableStacktrace = true
	logCfg.EncoderConfig.EncodeCaller = nil

	logger, err := logCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger, nil
}

func ChangeLogLevel(level zapcore.Level) (*zap.Logger, error) {
	logCfg.Level = zap.NewAtomicLevelAt(level)
	if level == zap.DebugLevel {
		logCfg.DisableStacktrace = false
		logCfg.EncoderConfig.EncodeCaller = zapcore.ShortCallerEncoder
	}

	logger, err := logCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger, nil
}

func AddMode(mode string) (*zap.Logger, error) {
	// Get the current logger configuration
	cfg := logCfg
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
	logCfg.Encoding = "nonColorConsole"
	logger, err := logCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("failed to build config for logger: %v", err)
	}
	return logger, nil
}
