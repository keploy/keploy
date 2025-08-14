package log

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestNew_FileErrors_001 tests the New function for file creation and permission errors.
func TestNew_FileErrors_001(t *testing.T) {
	// Arrange
	originalOsOpenFile := osOpenFile184
	originalOsChmod := osChmod184
	defer func() {
		osOpenFile184 = originalOsOpenFile
		osChmod184 = originalOsChmod
	}()

	osOpenFile184 = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return nil, fmt.Errorf("mocked file open error")
	}

	// Act
	logger, logFile, err := New()

	// Assert
	require.Error(t, err)
	assert.Nil(t, logger)
	assert.Nil(t, logFile)
	assert.Contains(t, err.Error(), "failed to open log file")
}

// TestNew_FilePermissionErrors_002 tests the New function for file permission errors.
func TestNew_FilePermissionErrors_002(t *testing.T) {
	// Arrange
	originalOsOpenFile := osOpenFile184
	originalOsChmod := osChmod184
	defer func() {
		osOpenFile184 = originalOsOpenFile
		osChmod184 = originalOsChmod
	}()

	osOpenFile184 = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return &os.File{}, nil
	}

	osChmod184 = func(name string, mode os.FileMode) error {
		return fmt.Errorf("mocked chmod error")
	}

	// Act
	logger, logFile, err := New()

	// Assert
	require.Error(t, err)
	assert.Nil(t, logger)
	assert.Nil(t, logFile)
	assert.Contains(t, err.Error(), "failed to set the log file permission to 777")
}

// TestNew_Success_003 tests the New function for successful logger creation.
func TestNew_Success_003(t *testing.T) {
	// Arrange
	originalOsOpenFile := osOpenFile184
	originalOsChmod := osChmod184
	defer func() {
		osOpenFile184 = originalOsOpenFile
		osChmod184 = originalOsChmod
	}()

	osOpenFile184 = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		return &os.File{}, nil
	}

	osChmod184 = func(name string, mode os.FileMode) error {
		return nil
	}

	// Act
	logger, logFile, err := New()

	// Assert
	require.NoError(t, err)
	assert.NotNil(t, logger)
	assert.NotNil(t, logFile)
}

// TestChangeLogLevel_Debug_004 tests the ChangeLogLevel function for changing log level to Debug.
func TestChangeLogLevel_Debug_004(t *testing.T) {
	// Act
	logger, err := ChangeLogLevel(zap.DebugLevel)

	// Assert
	require.NoError(t, err)
	assert.NotNil(t, logger)
	assert.Equal(t, zap.DebugLevel, LogCfg.Level.Level())
	assert.False(t, LogCfg.DisableStacktrace)
	assert.NotNil(t, LogCfg.EncoderConfig.EncodeCaller)
}

// TestAddMode_Success_006 tests the AddMode function for adding a mode to the logger.
func TestAddMode_Success_006(t *testing.T) {
	// Act
	logger, err := AddMode("test-mode")

	// Assert
	require.NoError(t, err)
	assert.NotNil(t, logger)
}

// TestChangeLogLevel_BuildError_234 tests the error handling path of ChangeLogLevel when zap.Config.Build fails.
func TestChangeLogLevel_BuildError_234(t *testing.T) {
	// Arrange
	originalCfg := LogCfg
	defer func() { LogCfg = originalCfg }()
	// Set an invalid output path to cause LogCfg.Build() to fail.
	LogCfg.OutputPaths = []string{"/invalid-path/that-will-fail/log.txt"}

	// Act
	logger, err := ChangeLogLevel(zap.DebugLevel)

	// Assert
	require.Error(t, err)
	assert.Nil(t, logger)
	assert.Contains(t, err.Error(), "failed to build config for logger")
}

// TestAddMode_BuildError_345 tests the error handling path of AddMode when zap.Config.Build fails.
func TestAddMode_BuildError_345(t *testing.T) {
	// Arrange
	originalCfg := LogCfg
	defer func() { LogCfg = originalCfg }()
	// Set an invalid output path to cause LogCfg.Build() to fail.
	LogCfg.OutputPaths = []string{"/invalid-path/that-will-fail/log.txt"}

	// Act
	logger, err := AddMode("test-mode")

	// Assert
	require.Error(t, err)
	assert.Nil(t, logger)
	assert.Contains(t, err.Error(), "failed to add mode to logger")
}

// TestChangeColorEncoding_Success_123 tests the happy path of ChangeColorEncoding.
func TestChangeColorEncoding_Success_123(t *testing.T) {
	// Arrange
	originalCfg := LogCfg
	defer func() { LogCfg = originalCfg }()
	// Ensure a known starting state
	LogCfg.Encoding = "colorConsole"

	// Act
	logger, err := ChangeColorEncoding()

	// Assert
	require.NoError(t, err)
	assert.NotNil(t, logger)
	assert.Equal(t, "nonColorConsole", LogCfg.Encoding)
}

// TestChangeColorEncoding_BuildError_456 tests the error handling path of ChangeColorEncoding when zap.Config.Build fails.
func TestChangeColorEncoding_BuildError_456(t *testing.T) {
	// Arrange
	originalCfg := LogCfg
	defer func() { LogCfg = originalCfg }()
	// Set an invalid output path to cause LogCfg.Build() to fail.
	LogCfg.OutputPaths = []string{"/invalid-path/that-will-fail/log.txt"}

	// Act
	logger, err := ChangeColorEncoding()

	// Assert
	require.Error(t, err)
	assert.Nil(t, logger)
	assert.Contains(t, err.Error(), "failed to build config for logger")
}
