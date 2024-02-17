package log

import (
	"log"
	"net/http"
	"os"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var Emoji = "\U0001F430" + " Keploy:"

var debugMode bool

func New() *zap.Logger {
	_ = zap.RegisterEncoder("colorConsole", func(config zapcore.EncoderConfig) (zapcore.Encoder, error) {
		return NewColor(config), nil
	})

	logCfg := zap.NewDevelopmentConfig()

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
			log.Println(Emoji, "failed to create log file", err)
			return nil
		}
	}

	// Check if the permission of the log file is 777, if not set it to 777.
	fileInfo, err := os.Stat("keploy-logs.txt")
	if err != nil {
		log.Println(Emoji, "failed to get the log file info", err)
		return nil
	}
	if fileInfo.Mode().Perm() != 0777 {
		// Set the permissions of the log file to 777.
		err = os.Chmod("keploy-logs.txt", 0777)
		if err != nil {
			log.Println(Emoji, "failed to set permissions of log file", err)
			return nil
		}
	}

	if debugMode {
		go func() {
			defer utils.HandlePanic()
			log.Println(http.ListenAndServe("localhost:6060", nil))
		}()

		logCfg.Level = zap.NewAtomicLevelAt(zap.DebugLevel)
		logCfg.DisableStacktrace = false
	} else {
		logCfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
		logCfg.DisableStacktrace = true
		logCfg.EncoderConfig.EncodeCaller = nil
	}

	logger, err := logCfg.Build()
	if err != nil {
		log.Panic(Emoji, "failed to start the log for the CLI", err)
		return nil
	}
	return logger
}

func DeleteLogs(logger *zap.Logger) {
	//Check if keploy-log.txt exists
	_, err := os.Stat("keploy-logs.txt")
	if os.IsNotExist(err) {
		return
	}
	//If it does, remove it.
	err = os.Remove("keploy-logs.txt")
	if err != nil {
		logger.Error("Error removing log file: %v\n", zap.String("error", err.Error()))
		return
	}
}
