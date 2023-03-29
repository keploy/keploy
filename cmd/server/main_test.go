package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/keploy/go-sdk/keploy"
	"go.keploy.io/server/server"
	"go.uber.org/zap"
)

// MakeFunctionRunOnRootFolder changes current directory to root when test file is executed
func MakeFunctionRunOnRootFolder() {
	logConf := zap.NewDevelopmentConfig()
	logConf.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	logger, err := logConf.Build()
	if err != nil {
		panic(err)
	}
	defer logger.Sync()
	ospath, err := os.Getwd()
	if err != nil {
		logger.Error("failed to get current directory path", zap.Error(err))
	}
	// already in root directory
	if !strings.Contains(ospath, "cmd/server") {
		return
	}

	// get the absolute path of listmonk root directory
	dir, err := filepath.Abs("../../")
	if err != nil {
		logger.Error("failed to get root directory path of listmonk", zap.Error(err))
	}

	// change current direstory to root
	err = os.Chdir(dir)
	if err != nil {
		logger.Error("failed to change current directory path to root listmonk", zap.Error(err))
	}
}

func TestKeploy(t *testing.T) {
	MakeFunctionRunOnRootFolder()
	// setup
	keploy.SetTestMode()

	// test the server
	if version == "" {
		version = getKeployVersion()
	}
	go server.Server(version)

	// teardown
	keploy.AssertTests(t)
}
