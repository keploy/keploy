package main

import (
	"testing"

	"context"
	"errors"
	"os"

	"github.com/spf13/cobra"
	userDb "go.keploy.io/server/v2/pkg/platform/yaml/configdb/user"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"gotest.tools/v3/assert"
)

// TestSetVersion_Default_101 ensures that if the global version variable is empty,
// it is set to the default "2-dev".
func TestSetVersion_Default_101(t *testing.T) {
	origVersion := version
	t.Cleanup(func() {
		version = origVersion
	})

	version = ""
	setVersion()
	assert.Equal(t, "2-dev", utils.Version)
}

// TestStart_LoggerError_456 tests the behavior of start when log.New fails.
func TestStart_LoggerError_456(t *testing.T) {
	origLogNew := logNew
	t.Cleanup(func() {
		logNew = origLogNew
	})

	logNew = func() (*zap.Logger, *os.File, error) {
		return nil, nil, errors.New("logger initialization failed")
	}

	// As the function just prints and returns, we don't have a direct way to assert
	// other than observing it doesn't panic. This test covers the error branch.
	start(context.Background())
}

// TestStart_ExecuteCmdGenericError_890 tests that the program does not exit on a generic command execution error.
func TestStart_ExecuteCmdGenericError_890(t *testing.T) {
	// Backup and restore mocks
	origLogNew := logNew
	origGetInstallationID := getInstallationID
	origExecuteCmd := executeCmd
	origOsExit := osExit
	t.Cleanup(func() {
		logNew = origLogNew
		getInstallationID = origGetInstallationID
		executeCmd = origExecuteCmd
		osExit = origOsExit
	})

	// Mock dependencies
	logNew = func() (*zap.Logger, *os.File, error) {
		return zap.NewNop(), nil, nil
	}
	getInstallationID = func(udb *userDb.Db, ctx context.Context) (string, error) {
		return "test-id", nil
	}
	executeCmd = func(cmd *cobra.Command) error {
		return errors.New("a generic error")
	}
	osExit = func(code int) {
		t.Fatalf("os.Exit(%d) was called unexpectedly", code)
	}

	start(context.Background())
}

// TestStart_DeleteLogsError_112 tests the deferred function handling when deleting the log file fails.
func TestStart_DeleteLogsError_112(t *testing.T) {
	// Backup and restore mocks
	origLogNew := logNew
	origGetInstallationID := getInstallationID
	origExecuteCmd := executeCmd
	origCloseFile := closeFile
	origDeleteFile := deleteFile
	origOsExit := osExit

	t.Cleanup(func() {
		logNew = origLogNew
		getInstallationID = origGetInstallationID
		executeCmd = origExecuteCmd
		closeFile = origCloseFile
		deleteFile = origDeleteFile
		osExit = origOsExit
	})

	// Mock dependencies
	logNew = func() (*zap.Logger, *os.File, error) {
		return zap.NewNop(), nil, nil
	}
	getInstallationID = func(udb *userDb.Db, ctx context.Context) (string, error) {
		return "test-id", nil
	}
	executeCmd = func(cmd *cobra.Command) error {
		return nil
	}
	closeFile = func(f *os.File) error {
		return nil
	}
	deleteFile = func(logger *zap.Logger, name string) error {
		if name == "keploy-logs.txt" {
			return errors.New("failed to delete logs")
		}
		return nil
	}
	osExit = func(code int) {
		t.Errorf("os.Exit(%d) was called unexpectedly", code)
	}

	start(context.Background())
}

// TestStart_DeleteDockerComposeError_223 tests the deferred function handling when deleting the temporary docker-compose file fails.
func TestStart_DeleteDockerComposeError_223(t *testing.T) {
	// Backup and restore mocks
	origLogNew := logNew
	origGetInstallationID := getInstallationID
	origExecuteCmd := executeCmd
	origCloseFile := closeFile
	origDeleteFile := deleteFile
	origOsExit := osExit

	t.Cleanup(func() {
		logNew = origLogNew
		getInstallationID = origGetInstallationID
		executeCmd = origExecuteCmd
		closeFile = origCloseFile
		deleteFile = origDeleteFile
		osExit = origOsExit
	})

	// Mock dependencies
	logNew = func() (*zap.Logger, *os.File, error) {
		return zap.NewNop(), nil, nil
	}
	getInstallationID = func(udb *userDb.Db, ctx context.Context) (string, error) {
		return "test-id", nil
	}
	executeCmd = func(cmd *cobra.Command) error {
		return nil
	}
	closeFile = func(f *os.File) error {
		return nil
	}
	deleteFile = func(logger *zap.Logger, name string) error {
		if name == "docker-compose-tmp.yaml" {
			return errors.New("failed to delete docker compose")
		}
		return nil
	}
	osExit = func(code int) {
		t.Errorf("os.Exit(%d) was called unexpectedly", code)
	}

	start(context.Background())
}
