package python

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestCreatePyCoverageConfig_FileCreationError_002 verifies that the function handles file creation errors correctly.
func TestCreatePyCoverageConfig_FileCreationError_002(t *testing.T) {
	// Arrange
	mockLogger := zap.NewNop()
	osCreate321 = func(name string) (*os.File, error) {
		return nil, fmt.Errorf("file creation error")
	}
	utilsLogError321 = func(logger *zap.Logger, err error, msg string, fields ...zap.Field) {
		assert.Equal(t, "file creation error", err.Error())
		assert.Equal(t, "failed to create .coveragerc file", msg)
	}

	// Act
	createPyCoverageConfig(mockLogger)
}

// TestCreatePyCoverageConfig_WriteStringError_003 verifies that the function handles errors when writing to the file.
func TestCreatePyCoverageConfig_WriteStringError_003(t *testing.T) {
	// Arrange
	originalOsCreate := osCreate321
	originalLogError := utilsLogError321
	defer func() {
		osCreate321 = originalOsCreate
		utilsLogError321 = originalLogError
	}()

	mockLogger := zap.NewNop()

	// Use os.Pipe to simulate a file that can error on write
	r, w, err := os.Pipe()
	require.NoError(t, err)

	osCreate321 = func(name string) (*os.File, error) {
		return w, nil
	}

	// Close the reader end. This will cause the WriteString call on the writer to fail.
	r.Close()

	logCalled := false
	utilsLogError321 = func(logger *zap.Logger, err error, msg string, fields ...zap.Field) {
		if msg == "failed to write to .coveragerc file" {
			logCalled = true
			assert.Error(t, err)
		}
	}

	// Act
	createPyCoverageConfig(mockLogger)
	// The function under test will defer w.Close(), so we don't need to close it here.

	// Assert
	assert.True(t, logCalled, "utils.LogError should have been called for WriteString error")
}
